package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	cmSync "github.com/oasisprotocol/oasis-core/go/common/sync"
	"github.com/oasisprotocol/oasis-core/go/config"
)

const (
	// retryInterval is the time interval between failed bundle downloads.
	retryInterval = 15 * time.Minute

	// requestTimeout is the time limit for http client requests.
	requestTimeout = time.Minute

	// maxMetadataSizeBytes is the maximum allowed metadata size in bytes.
	maxMetadataSizeBytes = 2 * 1024 // 2 KB

	// maxDefaultBundleSizeBytes is the maximum allowed default bundle size
	// in bytes.
	maxDefaultBundleSizeBytes = 20 * 1024 * 1024 // 20 MB
)

// ManifestStore is an interface that defines methods for storing manifests.
type ManifestStore interface {
	// HasManifest returns true iff the store already contains a manifest
	// with the given hash.
	HasManifest(hash hash.Hash) bool

	// AddManifest adds the provided manifest, whose components were extracted
	// to the specified directory, to the store.
	AddManifest(manifest *Manifest, dir string) error
}

// Manager is responsible for managing bundles.
type Manager struct {
	mu       sync.RWMutex
	startOne cmSync.One

	dataDir            string
	bundleDir          string
	maxBundleSizeBytes int64

	runtimeIDs map[common.Namespace]struct{}

	runtimeBaseURLs map[common.Namespace][]string
	globalBaseURLs  []string

	downloadCh    chan struct{}
	downloadQueue map[common.Namespace][]hash.Hash

	client *http.Client
	store  ManifestStore

	logger logging.Logger
}

// NewManager creates a new bundle manager.
func NewManager(dataDir string, runtimeIDs []common.Namespace, store ManifestStore) (*Manager, error) {
	logger := logging.GetLogger("runtime/bundle/manager")

	// Configure the HTTP client with a reasonable timeout.
	client := http.Client{
		Timeout: requestTimeout,
	}

	// Define a limit on the maximum allowed bundle size.
	bundleSize := int64(maxDefaultBundleSizeBytes)
	if size := config.GlobalConfig.Runtime.MaxBundleSize; size != "" {
		bundleSize = int64(config.ParseSizeInBytes(size))
	}

	// Validate global repository URLs.
	globalBaseURLs, err := validateAndNormalizeURLs(config.GlobalConfig.Runtime.Registries)
	if err != nil {
		return nil, err
	}

	// Validate each runtime's registry URLs.
	runtimeBaseURLs := make(map[common.Namespace][]string)
	for _, runtime := range config.GlobalConfig.Runtime.Runtimes {
		urls, err := validateAndNormalizeURLs(runtime.Registries)
		if err != nil {
			return nil, err
		}
		if len(urls) == 0 {
			continue
		}
		runtimeBaseURLs[runtime.ID] = urls
	}

	// Remember which runtimes to follow.
	runtimes := make(map[common.Namespace]struct{})
	for _, runtimeID := range runtimeIDs {
		runtimes[runtimeID] = struct{}{}
	}

	return &Manager{
		startOne:           cmSync.NewOne(),
		dataDir:            dataDir,
		bundleDir:          ExplodedPath(dataDir),
		maxBundleSizeBytes: bundleSize,
		runtimeIDs:         runtimes,
		globalBaseURLs:     globalBaseURLs,
		runtimeBaseURLs:    runtimeBaseURLs,
		downloadCh:         make(chan struct{}, 1),
		downloadQueue:      make(map[common.Namespace][]hash.Hash),
		client:             &client,
		store:              store,
		logger:             *logger,
	}, nil
}

// Start starts the bundle manager.
func (m *Manager) Start() {
	m.startOne.TryStart(m.run)
}

// Stop halts the bundle manager.
func (m *Manager) Stop() {
	m.startOne.TryStop()
}

func (m *Manager) run(ctx context.Context) {
	m.logger.Info("starting")

	// Ensure the bundle directory exists.
	if err := common.Mkdir(m.bundleDir); err != nil {
		m.logger.Error("failed to create bundle directory",
			"err", err,
			"dir", m.bundleDir,
		)
		return
	}

	// Extract bundles from the configuration.
	exploded, err := m.explodeBundles(config.GlobalConfig.Runtime.Paths)
	if err != nil {
		m.logger.Error("failed to explode bundles",
			"err", err,
		)
		return
	}

	// Load all manifests from the bundle directory.
	manifests, err := m.loadManifests()
	if err != nil {
		m.logger.Error("failed to load manifests",
			"err", err,
		)
		return
	}

	// Remove unneeded bundles and update the manifest map accordingly.
	manifests, err = m.cleanupBundles(manifests, exploded)
	if err != nil {
		m.logger.Error("failed to cleanup bundles",
			"err", err,
		)
		return
	}

	// Register the remaining manifests in the registry.
	err = m.registerManifests(manifests)
	if err != nil {
		m.logger.Error("failed to register manifests",
			"err", err,
		)
		return
	}

	// Start the main task responsible for managing bundles.
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-m.downloadCh:
		case <-ctx.Done():
			m.logger.Info("stopping")
			return
		}

		m.Download()
	}
}

// Queue updates the checksums of bundles pending download for the given runtime.
//
// Any existing checksums in the download queue for the given runtime are removed
// and replaced with the given ones.
func (m *Manager) Queue(runtimeID common.Namespace, manifestHashes []hash.Hash) {
	// Download bundles only for the configured runtimes.
	if _, ok := m.runtimeIDs[runtimeID]; !ok {
		return
	}

	// Download bundles only if at least one endpoint is configured.
	if len(m.globalBaseURLs) == 0 && len(m.runtimeBaseURLs[runtimeID]) == 0 {
		return
	}

	// Filter out bundles that have already been fetched.
	var hashes []hash.Hash
	for _, hash := range manifestHashes {
		if m.store.HasManifest(hash) {
			continue
		}
		hashes = append(hashes, hash)
	}

	// Update the queue.
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(hashes) == 0 {
		delete(m.downloadQueue, runtimeID)
		return
	}
	m.downloadQueue[runtimeID] = hashes

	// Trigger immediate download of new bundles.
	select {
	case m.downloadCh <- struct{}{}:
	default:
	}
}

// Download tries to download bundles in the queue.
func (m *Manager) Download() {
	for runtimeID := range m.runtimeIDs {
		m.downloadBundles(runtimeID)
	}
}

func (m *Manager) downloadBundles(runtimeID common.Namespace) {
	// Try to download queued bundles.
	m.mu.RLock()
	hashes := m.downloadQueue[runtimeID]
	m.mu.RUnlock()

	downloaded := make(map[hash.Hash]struct{})
	for _, hash := range hashes {
		if err := m.downloadBundle(runtimeID, hash); err != nil {
			m.logger.Error("failed to download bundle",
				"err", err,
				"runtime_id", runtimeID,
				"manifest_hash", hash.Hex(),
			)
			continue
		}
		downloaded[hash] = struct{}{}
	}

	// Remove downloaded bundles from the queue.
	m.mu.Lock()
	defer m.mu.Unlock()

	var pending []hash.Hash
	for _, hash := range m.downloadQueue[runtimeID] {
		if _, ok := downloaded[hash]; ok {
			continue
		}
		pending = append(pending, hash)
	}
	if len(pending) == 0 {
		delete(m.downloadQueue, runtimeID)
		return
	}
	m.downloadQueue[runtimeID] = pending
}

func (m *Manager) downloadBundle(runtimeID common.Namespace, manifestHash hash.Hash) error {
	var errs error

	for _, baseURLs := range [][]string{m.runtimeBaseURLs[runtimeID], m.globalBaseURLs} {
		for _, baseURL := range baseURLs {
			if err := m.tryDownloadBundle(manifestHash, baseURL); err != nil {
				errs = errors.Join(errs, err)
				continue
			}

			return nil
		}
	}

	return errs
}

func (m *Manager) tryDownloadBundle(manifestHash hash.Hash, baseURL string) error {
	metaURL, err := url.JoinPath(baseURL, manifestHash.Hex())
	if err != nil {
		m.logger.Error("failed to construct metadata URL",
			"err", err,
		)
		return fmt.Errorf("failed to construct metadata URL: %w", err)
	}

	bundleURL, err := m.fetchMetadata(metaURL)
	if err != nil {
		m.logger.Error("failed to download metadata",
			"err", err,
			"url", metaURL,
		)
		return fmt.Errorf("failed to download metadata: %w", err)
	}

	bundleURL, err = validateAndNormalizeURL(bundleURL)
	if err != nil {
		return err
	}

	src, err := m.fetchBundle(bundleURL)
	if err != nil {
		m.logger.Error("failed to download bundle",
			"err", err,
			"url", metaURL,
		)
		return fmt.Errorf("failed to download bundle: %w", err)
	}
	defer os.Remove(src)

	manifest, dir, err := m.explodeBundle(src, WithManifestHash(manifestHash))
	if err != nil {
		m.logger.Error("failed to explode bundle",
			"err", err,
			"src", src,
		)
		return err
	}

	if err := m.registerManifest(manifest, dir); err != nil {
		m.logger.Error("failed to register manifest",
			"err", err,
		)
		return fmt.Errorf("failed to register manifest: %w", err)
	}

	return nil
}

func (m *Manager) fetchMetadata(url string) (string, error) {
	m.logger.Info("downloading metadata",
		"url", url,
	)

	resp, err := m.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch metadata: invalid status code %d", resp.StatusCode)
	}

	limitedReader := io.LimitedReader{
		R: resp.Body,
		N: maxMetadataSizeBytes,
	}

	var buffer bytes.Buffer
	_, err = buffer.ReadFrom(&limitedReader)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("failed to read metadata content: %w", err)
	}
	metadata := strings.TrimSpace(buffer.String())

	m.logger.Info("metadata downloaded",
		"metadata", metadata,
	)

	return metadata, nil
}

func (m *Manager) fetchBundle(url string) (string, error) {
	m.logger.Info("downloading bundle",
		"url", url,
	)

	resp, err := m.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch bundle: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch bundle: invalid status code %d", resp.StatusCode)
	}

	// Copy to a temporary file. as downloaded bundles are unverified.
	file, err := os.CreateTemp("", fmt.Sprintf("oasis-bundle-*%s", FileExtension))
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer func() {
		file.Close()
		if err != nil {
			_ = os.Remove(file.Name())
		}
	}()

	limitedReader := io.LimitedReader{
		R: resp.Body,
		N: m.maxBundleSizeBytes,
	}

	if _, err = io.Copy(file, &limitedReader); err != nil {
		return "", fmt.Errorf("failed to save bundle: %w", err)
	}

	if limitedReader.N <= 0 {
		return "", fmt.Errorf("bundle exceeds size limit of %d bytes", m.maxBundleSizeBytes)
	}

	m.logger.Info("bundle downloaded",
		"url", url,
	)

	return file.Name(), nil
}

func (m *Manager) loadManifests() (map[string]*Manifest, error) {
	m.logger.Info("loading manifests")

	manifests := make(map[string]*Manifest)

	entries, err := os.ReadDir(m.bundleDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read bundle directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(m.bundleDir, entry.Name())

		b, err := os.ReadFile(filepath.Join(dir, manifestName))
		if err != nil {
			return nil, fmt.Errorf("failed to read manifest: %w", err)
		}

		var manifest Manifest
		if err = json.Unmarshal(b, &manifest); err != nil {
			return nil, fmt.Errorf("failed to parse manifest: %w", err)
		}

		m.logger.Info("manifest loaded",
			"name", manifest.Name,
			"hash", manifest.Hash(),
		)

		manifests[dir] = &manifest
	}

	return manifests, nil
}

func (m *Manager) cleanupBundles(manifests, exploded map[string]*Manifest) (map[string]*Manifest, error) {
	m.logger.Info("cleaning bundles")

	detached := make(map[hash.Hash]struct{})
	for _, manifest := range exploded {
		if manifest.IsDetached() {
			detached[manifest.Hash()] = struct{}{}
		}
	}

	shouldKeep := func(manifest *Manifest) bool {
		if _, ok := m.runtimeIDs[manifest.ID]; !ok {
			return false
		}
		if manifest.IsDetached() {
			if _, ok := detached[manifest.Hash()]; !ok {
				return false
			}
		}

		return true
	}

	retained := make(map[string]*Manifest)
	for dir, manifest := range manifests {
		if shouldKeep(manifest) {
			retained[dir] = manifest
			continue
		}

		if err := m.removeBundle(dir); err != nil {
			return nil, err
		}
	}

	return retained, nil
}

func (m *Manager) removeBundle(dir string) error {
	m.logger.Info("removing bundle",
		"dir", dir,
	)

	if err := os.RemoveAll(dir); err != nil {
		m.logger.Info("failed to remove bundle",
			"err", err,
			"path", dir,
		)
		return fmt.Errorf("failed to remove bundle: %w", err)
	}

	m.logger.Info("bundle removed",
		"path", dir,
	)

	return nil
}

func (m *Manager) explodeBundles(paths []string) (map[string]*Manifest, error) {
	m.logger.Info("exploding bundles")

	manifests := make(map[string]*Manifest)
	for _, path := range paths {
		manifest, dir, err := m.explodeBundle(path)
		if err != nil {
			return nil, err
		}
		manifests[dir] = manifest
	}

	return manifests, nil
}

func (m *Manager) explodeBundle(path string, opts ...OpenOption) (*Manifest, string, error) {
	m.logger.Info("exploding bundle",
		"path", path,
	)

	bnd, err := Open(path, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("failed to open bundle: %w", err)
	}
	defer bnd.Close()

	dir, err := bnd.WriteExploded(m.dataDir)
	if err != nil {
		return nil, "", fmt.Errorf("failed to explode bundle: %w", err)
	}

	m.logger.Info("bundle exploded",
		"dir", dir,
	)

	return bnd.Manifest, dir, nil
}

func (m *Manager) registerManifests(manifests map[string]*Manifest) error {
	m.logger.Info("registering manifests")

	// Register detached manifests first to ensure all components
	// are available before a regular manifest is added.
	for _, detached := range []bool{true, false} {
		for dir, manifest := range manifests {
			if manifest.IsDetached() != detached {
				continue
			}
			if err := m.registerManifest(manifest, dir); err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *Manager) registerManifest(manifest *Manifest, dir string) error {
	m.logger.Info("registering manifest",
		"name", manifest.Name,
		"hash", manifest.Hash(),
	)

	return m.store.AddManifest(manifest, dir)
}

func validateAndNormalizeURL(rawURL string) (string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL '%s': %w", rawURL, err)
	}
	return parsedURL.String(), nil
}

func validateAndNormalizeURLs(rawURLs []string) ([]string, error) {
	var normalizedURLs []string

	for _, rawURL := range rawURLs {
		normalizedURL, err := validateAndNormalizeURL(rawURL)
		if err != nil {
			return nil, err
		}
		normalizedURLs = append(normalizedURLs, normalizedURL)
	}

	return normalizedURLs, nil
}