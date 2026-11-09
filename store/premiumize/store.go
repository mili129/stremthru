package premiumize

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MunifTanjim/stremthru/core"
	"github.com/MunifTanjim/stremthru/internal/buddy"
	"github.com/MunifTanjim/stremthru/internal/cache"
	"github.com/MunifTanjim/stremthru/internal/request"
	"github.com/MunifTanjim/stremthru/internal/torrent_info"
	"github.com/MunifTanjim/stremthru/internal/torrent_stream"
	"github.com/MunifTanjim/stremthru/internal/util"
	"github.com/MunifTanjim/stremthru/store"
	"github.com/MunifTanjim/stremthru/store/stats"
)

type StoreClientConfig struct {
	HTTPClient       *http.Client
	UserAgent        string
	ParentFolderName string
}

type StoreClient struct {
	Name   store.StoreName
	client *APIClient
	config *StoreClientConfig

	listMagnetsCache    cache.Cache[[]store.ListMagnetsDataItem]
	parentFolderIdCache cache.Cache[string]
}

func NewStoreClient(config *StoreClientConfig) *StoreClient {
	if config.ParentFolderName == "" {
		config.ParentFolderName = "stremthru"
	}

	c := &StoreClient{}
	c.client = NewAPIClient(&APIClientConfig{
		HTTPClient: config.HTTPClient,
		UserAgent:  config.UserAgent,
	})
	c.Name = store.StoreNamePremiumize
	c.config = config

	c.listMagnetsCache = func() cache.Cache[[]store.ListMagnetsDataItem] {
		return cache.NewCache[[]store.ListMagnetsDataItem](&cache.CacheConfig{
			Name:     "store:premiumize:listMagnets",
			Lifetime: 1 * time.Minute,
		})
	}()

	c.parentFolderIdCache = cache.NewLRUCache[string](&cache.CacheConfig{
		Name:     "store:premiumize:parentFolderId",
		Lifetime: 15 * time.Minute,
	})

	return c
}

func (c *StoreClient) getCacheKey(params request.Context, key string) string {
	return params.GetAPIKey(c.client.apiKey) + ":" + key
}

func (c *StoreClient) getParentFolderId(apiKey string) (string, error) {
	parentFolderId := ""
	if c.parentFolderIdCache.Get(apiKey, &parentFolderId) {
		return parentFolderId, nil
	}

	lf_params := &ListFoldersParams{}
	lf_params.APIKey = apiKey
	lf_res, err := c.client.ListFolders(lf_params)
	if err != nil {
		return "", err
	}
	for _, folder := range lf_res.Data.Content {
		if folder.Name == c.config.ParentFolderName {
			parentFolderId = folder.Id
			break
		}
	}

	if parentFolderId == "" {
		cf_params := &CreateFolderParams{Name: c.config.ParentFolderName}
		cf_params.APIKey = apiKey
		cf_res, err := c.client.CreateFolder(cf_params)
		if err != nil {
			return "", err
		}
		parentFolderId = cf_res.Data.Id
	}

	if parentFolderId == "" {
		return "", errors.New("failed to resolve parent folder id")
	}

	if err := c.parentFolderIdCache.Add(apiKey, parentFolderId); err != nil {
		slog.Error("[premiumize] failed to cache parent folder id", "error", err)
	}

	return parentFolderId, nil
}

func (c *StoreClient) getFolderByName(apiKey string, folderName string) (*CreateFolderData, error) {
	parentFolderId := ""
	if folderName != c.config.ParentFolderName {
		// resolve parent-folder id, if querying for non-parent-folder
		id, err := c.getParentFolderId(apiKey)
		if err != nil {
			return nil, err
		}
		parentFolderId = id
	}

	params := &ListFoldersParams{}
	params.APIKey = apiKey
	if parentFolderId != "" {
		params.Id = parentFolderId
	}
	res, err := c.client.ListFolders(params)
	if err != nil {
		return nil, err
	}

	for _, folder := range res.Data.Content {
		if folder.Name == folderName {
			return &CreateFolderData{Id: folder.Id}, nil
		}
	}

	return nil, nil
}

func (c *StoreClient) ensureFolder(apiKey string, name string) (*CreateFolderData, error) {
	if folder, err := c.getFolderByName(apiKey, name); err != nil {
		return nil, err
	} else if folder != nil {
		return &CreateFolderData{Id: folder.Id}, nil
	}

	parentFolderId, err := c.getParentFolderId(apiKey)
	if err != nil {
		return nil, err
	}

	cf_params := &CreateFolderParams{Name: name}
	cf_params.ParentId = parentFolderId
	cf_params.APIKey = apiKey
	cf_res, err := c.client.CreateFolder(cf_params)
	if err != nil {
		return nil, err
	}
	return &cf_res.Data, nil
}

func (c *StoreClient) GetName() store.StoreName {
	return c.Name
}

func (c *StoreClient) GetUser(params *store.GetUserParams) (*store.User, error) {
	start := time.Now()
	res, err := c.client.GetAccountInfo(&GetAccountInfoParams{
		Ctx: params.Ctx,
	})
	stats.Record(c.Name, "get_user", time.Since(start), err != nil)
	if err != nil {
		return nil, err
	}
	data := &store.User{
		Id:    res.Data.CustomerId,
		Email: "",
	}
	if res.Data.PremiumUntil != 0 {
		data.SubscriptionStatus = store.UserSubscriptionStatusPremium
		if res.Data.PremiumUntil < int(time.Now().Unix()) {
			data.SubscriptionStatus = store.UserSubscriptionStatusExpired
		}
	} else {
		data.SubscriptionStatus = store.UserSubscriptionStatusExpired
	}
	return data, err
}

func (c *StoreClient) getCachedMagnetFiles(apiKey string, magnet string, includeLink bool) ([]store.MagnetFile, error) {
	params := &CreateDirectDownloadLinkParams{Src: magnet}
	params.APIKey = apiKey
	start := time.Now()
	res, err := c.client.CreateDirectDownloadLink(params)
	stats.Record(c.Name, "generate_torz_link", time.Since(start), err != nil)
	if err != nil {
		return nil, err
	}
	files := []store.MagnetFile{}
	source := string(c.GetName().Code())
	for idx, f := range res.Data.Content {
		// [local patch] a single-file torrent has no root folder, so GetPath()
		// returns "" and the file list was never stored (torrent_stream wants a
		// path starting with "/"): it was fetched again every day and never reused.
		fPath := f.GetPath()
		if fPath == "" && strings.Trim(f.Path, "/") != "" {
			fPath = "/" + f.GetName()
		}
		file := &store.MagnetFile{
			Idx:    idx,
			Name:   f.GetName(),
			Path:   fPath,
			Size:   f.Size,
			Source: source,
		}
		if includeLink {
			file.Link = f.Link
			if f.StreamLink != "" {
				file.Link = f.StreamLink
			}
		}
		files = append(files, *file)
	}
	return files, nil
}

func (c *StoreClient) checkMagnet(params *store.CheckMagnetParams, includeLinkAndPath bool) (*store.CheckMagnetData, error) {
	magnetByHash := make(map[string]core.MagnetLink, len(params.Magnets))
	hashes := make([]string, 0, len(params.Magnets))

	missingHashes := []string{}

	for _, m := range params.Magnets {
		magnet, err := core.ParseMagnetLink(m)
		if err != nil {
			return nil, err
		}
		if len(magnet.Hash) != 40 {
			continue
		}
		magnetByHash[magnet.Hash] = magnet
		hashes = append(hashes, magnet.Hash)
	}

	foundItemByHash := map[string]store.CheckMagnetDataItem{}

	if !includeLinkAndPath {
		if data, err := buddy.CheckMagnet(c, hashes, params.GetAPIKey(c.client.apiKey), params.ClientIP, params.SId, true); err != nil {
			return nil, err
		} else {
			for _, item := range data.Items {
				foundItemByHash[item.Hash] = item
			}
		}

		if params.LocalOnly {
			data := &store.CheckMagnetData{
				Items: []store.CheckMagnetDataItem{},
			}

			for _, hash := range hashes {
				if item, ok := foundItemByHash[hash]; ok {
					data.Items = append(data.Items, item)
				}
			}
			return data, nil
		}
	}

	for _, hash := range hashes {
		if _, ok := foundItemByHash[hash]; !ok {
			missingHashes = append(missingHashes, hash)
		}
	}

	itemByHash := map[string]store.AddMagnetData{}
	partial := false
	if len(missingHashes) > 0 {
		chunkCount := len(missingHashes)/100 + 1
		cItems := make([][]store.AddMagnetData, chunkCount)
		errs := make([]error, chunkCount)

		// [local patch] file lists already stored for these hashes. A torrent's
		// files never change, so a stale magnet that is still cached reuses them
		// instead of a new directdl call. Not for playback, which needs links.
		knownFilesByHash := map[string]torrent_stream.Files{}
		if !includeLinkAndPath {
			if fbh, err := torrent_stream.GetFilesByHashes(missingHashes); err == nil {
				knownFilesByHash = fbh
			} else {
				log.Warn("failed to get stored magnet files", "error", err)
			}
		}

		// [local patch] at most premiumizeCheckConcurrency cache/check calls at
		// once; a big title used to send up to 20 together (bursts -> 429).
		// The semaphore is shared by all requests (it was per request, so two
		// titles opened together doubled the limit). Playback does not wait.
		var wg sync.WaitGroup
		for i, cMissingHashes := range slices.Collect(slices.Chunk(missingHashes, 100)) {
			wg.Go(func() {
				if !includeLinkAndPath {
					premiumizeCheckSem <- struct{}{}
					defer func() { <-premiumizeCheckSem }()
				}
				ccParams := &CheckCacheParams{
					Items: cMissingHashes,
				}
				ccParams.APIKey = params.APIKey
				start := time.Now()
				res, err := c.client.CheckCache(ccParams)
				// [local patch] Premiumize rate limits are short bursts: retry a
				// 429 twice (after 1 s, then 2 s) instead of returning nothing.
				for attempt := 1; attempt <= 2 && err != nil && strings.Contains(err.Error(), "TOO_MANY_REQUESTS"); attempt++ {
					time.Sleep(time.Duration(attempt) * time.Second)
					res, err = c.client.CheckCache(ccParams)
				}
				stats.Record(c.Name, "check_torz", time.Since(start), err != nil)
				if err != nil {
					errs[i] = err
					return
				}

				items := []store.AddMagnetData{}
				for idx, is_cached := range res.Data.Response {
					magnet, err := core.ParseMagnetLink(ccParams.Items[idx])
					if err != nil {
						errs[i] = err
						return
					}
					size, err := res.Data.Filesize[idx].Int64()
					if err != nil {
						size = -1
					}
					item := store.AddMagnetData{
						Name:   res.Data.Filename[idx],
						Size:   size,
						Magnet: magnet.Link,
						Hash:   magnet.Hash,
						Status: store.MagnetStatusUnknown,
						Files:  []store.MagnetFile{},
					}

					if is_cached {
						item.Status = store.MagnetStatusCached
					}

					items = append(items, item)
				}

				cItems[i] = items
			})
		}
		wg.Wait()
		// [local patch] computed after Wait (was set from the goroutines)
		hasError := slices.ContainsFunc(errs, func(err error) bool { return err != nil })

		// [local patch] a failed chunk no longer discards the other chunks;
		// fail only if every chunk failed.
		if hasError && !slices.ContainsFunc(cItems, func(items []store.AddMagnetData) bool { return items != nil }) {
			return nil, errors.Join(errs...)
		}

		// [local patch] fetch file lists only for the cached magnets allowed by
		// STREMTHRU_PREMIUMIZE_FILES_LIMITS, concurrently. Magnets without files
		// are not stored in magnet_cache, so skipped or failed ones are
		// checked again next time.
		type cachedRef struct{ chunk, idx int }
		refs := []cachedRef{}
		candidates := []filesCandidate{}
		reused := 0
		for ci, items := range cItems {
			for idx, item := range items {
				if item.Status == store.MagnetStatusCached {
					// [local patch] stored file list from Premiumize: no directdl, not
					// counted in the limits (lists from other stores may differ in Idx)
					if known := knownFilesByHash[strings.ToLower(item.Hash)]; len(known) > 0 && !slices.ContainsFunc(known, func(f torrent_stream.File) bool { return f.Source != string(c.GetName().Code()) }) {
						if files := known.ToStoreMagnetFiles(strings.ToLower(item.Hash)); len(files) > 0 {
							cItems[ci][idx].Files = files
							reused++
							continue
						}
					}
					refs = append(refs, cachedRef{ci, idx})
					candidates = append(candidates, filesCandidate{name: item.Name, size: item.Size, hint: params.ResolutionHints[strings.ToLower(item.Hash)]})
				}
			}
		}
		// [local patch] includeLinkAndPath is true only from AddMagnet (playback):
		// the user picked this torrent, so always list its files. The limits
		// are meant for the big availability checks, and a "." name without a
		// resolution hint would otherwise be skipped and playback would fail
		// with no video files.
		limits := premiumizeFilesLimits
		if includeLinkAndPath {
			limits = nil
		}
		selected := selectForFiles(candidates, limits)
		fileErrs := make([]error, len(selected))
		// [local patch] shared by all requests, like premiumizeCheckSem
		var fwg sync.WaitGroup
		for j, sel := range selected {
			ref := refs[sel]
			fwg.Go(func() {
				if !includeLinkAndPath {
					premiumizeFilesSem <- struct{}{}
					defer func() { <-premiumizeFilesSem }()
				}
				item := &cItems[ref.chunk][ref.idx]
				files, err := c.getCachedMagnetFiles(params.APIKey, item.Magnet, includeLinkAndPath)
				if err != nil {
					fileErrs[j] = err
					return
				}
				item.Files = files
			})
		}
		fwg.Wait()
		failed := 0
		for _, err := range fileErrs {
			if err != nil {
				failed++
			}
		}
		if hasError || failed > 0 || len(selected) < len(refs) || reused > 0 {
			log.Info("cached magnet files", "hashes", len(missingHashes), "cached", len(refs)+reused, "reused", reused, "fetched", len(selected)-failed, "skipped", len(refs)-len(selected), "failed", failed, "first_error", firstError(append(fileErrs, errs...)))
		}
		// [local patch] reused lists are still good when every new listing failed
		if failed > 0 && failed == len(selected) && reused == 0 {
			return nil, errors.Join(fileErrs...)
		}
		// [local patch] tell the client the answer is incomplete (a cache/check
		// chunk or a file listing failed), so it does not remember it as final.
		partial = hasError || failed > 0

		for _, items := range cItems {
			for _, item := range items {
				itemByHash[strings.ToLower(item.Hash)] = item
			}
		}
	}

	data := &store.CheckMagnetData{
		Items:   []store.CheckMagnetDataItem{},
		Partial: partial,
	}
	tInfos := []buddy.TorrentInfoInput{}
	// [local patch] hashes Premiumize answered as not cached; stored in
	// magnet_cache so they are not asked again until the uncached stale time
	// (STREMTHRU_STORE_CONTENT_CACHED_STALE_TIME). Before, only cached ones
	// were stored, so every check asked again about all the others.
	notCached := map[string]bool{}
	for _, hash := range hashes {
		if item, ok := foundItemByHash[hash]; ok {
			data.Items = append(data.Items, item)
			continue
		}

		m := magnetByHash[hash]
		item := store.CheckMagnetDataItem{
			Hash:   m.Hash,
			Magnet: m.Link,
			Status: store.MagnetStatusUnknown,
			Files:  []store.MagnetFile{},
		}
		tInfo := buddy.TorrentInfoInput{
			Hash: hash,
		}
		if it, ok := itemByHash[hash]; ok {
			if it.Status != store.MagnetStatusCached {
				notCached[hash] = false
			}
			item.Name = it.Name
			item.Size = it.Size
			tInfo.TorrentTitle = it.Name
			tInfo.Size = it.Size
			item.Status = it.Status
			for _, f := range it.Files {
				file := torrent_stream.File{
					Idx:    f.Idx,
					Name:   f.Name,
					Path:   f.Path,
					Size:   f.Size,
					Source: f.Source,
				}
				mFile := store.MagnetFile{
					Idx:    file.Idx,
					Name:   file.Name,
					Path:   file.Path,
					Size:   file.Size,
					Source: file.Source,
				}
				if includeLinkAndPath {
					mFile.Path = f.Path
					mFile.Link = f.Link
				}
				tInfo.Files = append(tInfo.Files, file)
				item.Files = append(item.Files, mFile)
			}
		}
		tInfos = append(tInfos, tInfo)
		data.Items = append(data.Items, item)
	}
	go buddy.BulkTrackMagnet(c, tInfos, notCached, "", params.GetAPIKey(c.client.apiKey))
	return data, nil
}

func (c *StoreClient) CheckMagnet(params *store.CheckMagnetParams) (*store.CheckMagnetData, error) {
	return c.checkMagnet(params, false)
}

func getTransferById(c *StoreClient, apiKey string, id string) (*ListTransfersDataItem, error) {
	params := &ListTransfersParams{}
	params.APIKey = apiKey
	start := time.Now()
	res, err := c.client.ListTransfers(params)
	stats.Record(c.Name, "list_transfers", time.Since(start), err != nil)
	if err != nil {
		return nil, err
	}
	for _, transfer := range res.Data.Transfers {
		if transfer.Id == id {
			return &transfer, nil
		}
	}
	return nil, nil
}

// [local patch] number of parallel directdl calls when listing files of
// cached magnets. Override with STREMTHRU_PREMIUMIZE_FILES_CONCURRENCY.
var premiumizeFilesConcurrency = func() int {
	if v, err := strconv.Atoi(os.Getenv("STREMTHRU_PREMIUMIZE_FILES_CONCURRENCY")); err == nil && v > 0 {
		return v
	}
	return 8
}()

// [local patch] parallel cache/check calls (100 hashes each). Override with
// STREMTHRU_PREMIUMIZE_CHECK_CONCURRENCY.
var premiumizeCheckConcurrency = func() int {
	if v, err := strconv.Atoi(os.Getenv("STREMTHRU_PREMIUMIZE_CHECK_CONCURRENCY")); err == nil && v > 0 {
		return v
	}
	return 4
}()

// [local patch] one semaphore each for the whole process, not per request
var premiumizeCheckSem = make(chan struct{}, premiumizeCheckConcurrency)
var premiumizeFilesSem = make(chan struct{}, premiumizeFilesConcurrency)

// [local patch] create the account folder for cached magnets on AddMagnet.
var premiumizeCachedFolders = os.Getenv("STREMTHRU_PREMIUMIZE_CACHED_FOLDERS") == "true"

type CachedMagnetId string

const CachedMagnetIdPrefix = "premiumize:cached:magnet:"

func (id CachedMagnetId) isValid() bool {
	return strings.HasPrefix(string(id), CachedMagnetIdPrefix)
}

func (id CachedMagnetId) toId(value string) CachedMagnetId {
	if id.isValid() {
		return id
	}
	return CachedMagnetId(CachedMagnetIdPrefix + value)
}

func (id CachedMagnetId) toString() string {
	return string(id)
}

func (id CachedMagnetId) toHash() string {
	return strings.TrimPrefix(string(id), CachedMagnetIdPrefix)
}

func listFolderFlat(c *StoreClient, apiKey string, folderId string, result []store.MagnetFile, parent *store.MagnetFile) ([]store.MagnetFile, error) {
	if result == nil {
		result = []store.MagnetFile{}
	}

	params := &ListFoldersParams{Id: folderId}
	params.APIKey = apiKey
	c_res, err := c.client.ListFolders(params)
	if err != nil {
		return nil, err
	}

	idx := 0
	if parent != nil {
		idx = parent.Idx
	}

	source := string(c.GetName().Code())
	for _, f := range c_res.Data.Content {
		file := &store.MagnetFile{
			Idx:    idx,
			Name:   f.Name,
			Path:   "/" + f.Name,
			Size:   f.Size,
			Link:   f.Link,
			Source: source,
		}

		if f.StreamLink != "" {
			file.Link = f.StreamLink
		}

		if parent != nil {
			file.Path = path.Join(parent.Path, file.Name)
		}

		if f.Type == FolderItemTypeFolder {
			result, err = listFolderFlat(c, apiKey, f.Id, result, file)
			if err != nil {
				return nil, err
			}
			if len(result) > 0 {
				idx = result[len(result)-1].Idx + 1
			}
		} else {
			result = append(result, *file)
			idx++
		}
	}

	return result, nil
}

func (c *StoreClient) AddMagnet(params *store.AddMagnetParams) (*store.AddMagnetData, error) {
	var magnet *core.MagnetLink
	var isPrivate bool
	if params.Magnet != "" {
		m, err := core.ParseMagnetLink(params.Magnet)
		if err != nil {
			return nil, err
		}
		magnet = &m
	} else {
		mi, mii, err := params.GetTorrentMeta()
		if err != nil {
			return nil, err
		}
		isPrivate = util.PtrToBool(mii.Private, false)
		m, err := core.ParseMagnetLink(mi.HashInfoBytes().HexString())
		if err != nil {
			return nil, err
		}
		magnet = &m
	}

	cm_res, err := c.checkMagnet(&store.CheckMagnetParams{
		Ctx:     params.Ctx,
		Magnets: []string{magnet.Link},
	}, true)
	if err != nil {
		return nil, err
	}

	cm := cm_res.Items[0]

	// already cached, no need to download
	if cm.Status == store.MagnetStatusCached {
		id := CachedMagnetId("").toId(magnet.Hash).toString()

		// [local patch] the empty "premiumize:cached:magnet:<hash>" folder only
		// makes the magnet show up in ListMagnets (account library). Playback
		// does not need it: it costs 1-2 Premiumize calls per play and leaves an
		// empty folder in the account. Set STREMTHRU_PREMIUMIZE_CACHED_FOLDERS=true
		// to create it again.
		if premiumizeCachedFolders {
			if _, err = c.ensureFolder(params.APIKey, id); err != nil {
				return nil, err
			}
		}

		name := magnet.Name
		if cm.Name != "" {
			name = cm.Name
		}
		data := &store.AddMagnetData{
			Id:      id,
			Hash:    magnet.Hash,
			Magnet:  magnet.Link,
			Name:    name,
			Status:  store.MagnetStatusDownloaded,
			Size:    cm.Size,
			Private: isPrivate,
			Files:   cm.Files,
			AddedAt: time.Unix(0, 0).UTC(),
		}

		if data.Size <= 0 {
			data.Size = 0
			for i := range cm.Files {
				data.Size += cm.Files[i].Size
			}
		}

		c.listMagnetsCache.Remove(c.getCacheKey(params, ""))

		return data, nil
	}

	// not cached, need to download
	folder, err := c.ensureFolder(params.APIKey, magnet.Hash)
	if err != nil {
		return nil, err
	}

	ct_params := &CreateTransferParams{
		Ctx:      params.Ctx,
		FolderId: folder.Id,
	}
	if params.Magnet != "" {
		ct_params.Src = magnet.RawLink
	} else {
		ct_params.File = params.Torrent
	}
	start := time.Now()
	ct_res, err := c.client.CreateTransfer(ct_params)
	stats.Record(c.Name, "add_torz", time.Since(start), err != nil)
	if err != nil {
		return nil, err
	}

	data := &store.AddMagnetData{
		Id:      ct_res.Data.Id,
		Hash:    magnet.Hash,
		Magnet:  magnet.Link,
		Name:    ct_res.Data.Name,
		Size:    -1,
		Status:  store.MagnetStatusQueued,
		Private: isPrivate,
		AddedAt: time.Now().UTC(),
	}

	transfer, err := getTransferById(c, params.APIKey, data.Id)
	if err != nil {
		return nil, err
	}
	if transfer == nil {
		c.listMagnetsCache.Remove(c.getCacheKey(params, ""))

		return data, nil
	}

	if transfer.Status == TransferStatusFinished {
		data.Status = store.MagnetStatusDownloaded

		files, err := listFolderFlat(c, params.APIKey, folder.Id, nil, nil)
		if err != nil {
			return nil, err
		}

		data.Files = files

		data.Size = 0
		for i := range files {
			data.Size += files[i].Size
		}
	}

	c.listMagnetsCache.Remove(c.getCacheKey(params, ""))

	return data, nil
}

func getMagnetStatsForTransfer(transfer *ListTransfersDataItem) store.MagnetStatus {
	if transfer.Status == TransferStatusFinished {
		return store.MagnetStatusDownloaded
	}
	if transfer.Status == TransferStatusRunning {
		if transfer.Progress > 0 {
			return store.MagnetStatusDownloading
		}
		return store.MagnetStatusQueued
	}
	return store.MagnetStatusUnknown
}

func (c *StoreClient) GetMagnet(params *store.GetMagnetParams) (*store.GetMagnetData, error) {
	if CachedMagnetId(params.Id).isValid() {
		magnet, err := core.ParseMagnetLink(CachedMagnetId(params.Id).toHash())
		if err != nil {
			return nil, err
		}
		files, err := c.getCachedMagnetFiles(params.APIKey, magnet.Link, true)
		if err != nil {
			return nil, err
		}
		name := magnet.Name
		size := int64(-1)
		if infoByHash, err := torrent_info.GetBasicInfoByHash([]string{magnet.Hash}); err == nil {
			if info, ok := infoByHash[magnet.Hash]; ok {
				name = info.TorrentTitle
				size = info.Size
			}
		} else {
			log.Warn("failed to get basic info by hash", "error", err, "hash", magnet.Hash)
		}
		if size <= 0 {
			for i := range files {
				size += files[i].Size
			}
		}
		data := &store.GetMagnetData{
			Id:      params.Id,
			Hash:    magnet.Hash,
			Name:    name,
			Size:    size,
			Status:  store.MagnetStatusDownloaded,
			Files:   files,
			AddedAt: time.Unix(0, 0).UTC(),
		}

		return data, nil
	}

	transfer, err := getTransferById(c, params.APIKey, params.Id)
	if err != nil {
		return nil, err
	}
	if transfer == nil {
		err := core.NewAPIError("not found")
		err.StatusCode = http.StatusNotFound
		err.StoreName = string(store.StoreNamePremiumize)
		return nil, err
	}

	magnet, err := core.ParseMagnetLink(transfer.Src)
	if err != nil {
		return nil, err
	}
	name := transfer.Name
	size := int64(-1)
	if infoByHash, err := torrent_info.GetBasicInfoByHash([]string{magnet.Hash}); err == nil {
		if info, ok := infoByHash[magnet.Hash]; ok {
			size = info.Size
		}
	} else {
		log.Warn("failed to get basic info by hash", "error", err, "hash", magnet.Hash)
	}
	data := &store.GetMagnetData{
		Id:      transfer.Id,
		Hash:    magnet.Hash,
		Name:    name,
		Size:    size,
		Status:  getMagnetStatsForTransfer(transfer),
		AddedAt: transfer.GetAddedAt(),
	}
	if transfer.Status == TransferStatusFinished {
		files, err := listFolderFlat(c, params.APIKey, transfer.FolderId, nil, &store.MagnetFile{
			Path: "/",
		})
		if err != nil {
			return nil, err
		}
		data.Files = files
		if data.Size <= 0 {
			data.Size = 0
			for i := range files {
				data.Size += files[i].Size
			}
		}
	}

	return data, nil
}

func (c *StoreClient) ListMagnets(params *store.ListMagnetsParams) (*store.ListMagnetsData, error) {
	lm := []store.ListMagnetsDataItem{}
	if !c.listMagnetsCache.Get(c.getCacheKey(params, ""), &lm) {
		start := time.Now()
		sf_res, err := c.client.SearchFolders(&SearchFoldersParams{
			Ctx:   params.Ctx,
			Query: CachedMagnetIdPrefix,
		})
		stats.Record(c.Name, "search_folders", time.Since(start), err != nil)
		if err != nil {
			return nil, err
		}

		start = time.Now()
		lt_res, err := c.client.ListTransfers(&ListTransfersParams{
			Ctx: params.Ctx,
		})
		stats.Record(c.Name, "list_transfers", time.Since(start), err != nil)
		if err != nil {
			return nil, err
		}

		items := []store.ListMagnetsDataItem{}

		hashes := make([]string, len(sf_res.Data.Content))
		for i, m := range sf_res.Data.Content {
			hash := CachedMagnetId(m.Name).toHash()
			hashes[i] = hash
			item := &store.ListMagnetsDataItem{
				Id:      m.Name,
				Hash:    hash,
				Name:    "",
				Size:    -1,
				Status:  store.MagnetStatusDownloaded,
				AddedAt: m.GetAddedAt(),
			}

			items = append(items, *item)
		}

		if infoByHash, err := torrent_info.GetBasicInfoByHash(hashes); err == nil {
			for i, hash := range hashes {
				if info, ok := infoByHash[hash]; ok {
					item := &items[i]
					if item.Hash == hash && item.Name == "" {
						item.Name = info.TorrentTitle
						item.Size = info.Size
					}
				}
			}
		} else {
			log.Warn("failed to get basic info by hash", "error", err, "count", len(hashes))
		}

		for _, t := range lt_res.Data.Transfers {
			if !strings.HasPrefix(t.Src, "magnet:") {
				continue
			}
			magnet, err := core.ParseMagnetLink(t.Src)
			if err != nil {
				return nil, err
			}
			item := &store.ListMagnetsDataItem{
				Id:      t.Id,
				Hash:    magnet.Hash,
				Name:    t.Name,
				Size:    -1,
				Status:  getMagnetStatsForTransfer(&t),
				AddedAt: t.GetAddedAt(),
			}

			items = append(items, *item)
		}

		lm = items
		c.listMagnetsCache.Add(c.getCacheKey(params, ""), items)
	}

	totalItems := len(lm)
	startIdx := min(params.Offset, totalItems)
	endIdx := min(startIdx+params.Limit, totalItems)
	items := lm[startIdx:endIdx]

	data := &store.ListMagnetsData{
		Items:      items,
		TotalItems: totalItems,
	}

	return data, nil
}

func (c *StoreClient) deleteTransferById(apiKey string, transferId string) error {
	transfer, err := getTransferById(c, apiKey, transferId)
	if err != nil {
		return err
	}
	if transfer == nil {
		return nil
	}

	dt_params := &DeleteTransferParams{Id: transferId}
	dt_params.APIKey = apiKey
	start := time.Now()
	_, err = c.client.DeleteTransfer(dt_params)
	stats.Record(c.Name, "remove_transfer", time.Since(start), err != nil)
	if err != nil {
		return err
	}

	return nil
}

func (c *StoreClient) RemoveMagnet(params *store.RemoveMagnetParams) (*store.RemoveMagnetData, error) {
	if CachedMagnetId(params.Id).isValid() {
		folder, err := c.getFolderByName(params.APIKey, CachedMagnetId(params.Id).toString())
		if err != nil {
			return nil, err
		}

		data := &store.RemoveMagnetData{Id: params.Id}

		if folder == nil {
			return data, nil
		}

		start := time.Now()
		_, err = c.client.DeleteFolder(&DeleteFolderParams{
			Ctx: params.Ctx,
			Id:  folder.Id,
		})
		stats.Record(c.Name, "remove_folder", time.Since(start), err != nil)
		if err != nil {
			return nil, err
		}

		c.listMagnetsCache.Remove(c.getCacheKey(params, ""))

		return data, nil
	}

	err := c.deleteTransferById(params.APIKey, params.Id)
	if err != nil {
		return nil, err
	}

	c.listMagnetsCache.Remove(c.getCacheKey(params, ""))

	data := &store.RemoveMagnetData{Id: params.Id}
	return data, nil
}

func (c *StoreClient) GenerateLink(params *store.GenerateLinkParams) (*store.GenerateLinkData, error) {
	data := &store.GenerateLinkData{Link: params.Link}
	return data, nil
}
