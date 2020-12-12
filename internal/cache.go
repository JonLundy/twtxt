package internal

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/jointwt/twtxt"
	"github.com/jointwt/twtxt/types"
	"github.com/jointwt/twtxt/types/retwt"
)

const (
	feedCacheFile = "cache"
)

// Cached ...
type Cached struct {
	mu           sync.RWMutex
	cache        types.TwtMap
	Twts         types.Twts
	Lastmodified string
}

// Lookup ...
func (cached *Cached) Lookup(hash string) (types.Twt, bool) {
	cached.mu.RLock()
	twt, ok := cached.cache[hash]
	cached.mu.RUnlock()
	if ok {
		return twt, true
	}

	for _, twt := range cached.Twts {
		if twt.Hash() == hash {
			cached.mu.Lock()
			if cached.cache == nil {
				cached.cache = make(map[string]types.Twt)
			}
			cached.cache[hash] = twt
			cached.mu.Unlock()
			return twt, true
		}
	}

	return &types.NilTwt{}, false
}

// OldCache ...
type OldCache map[string]*Cached

// Cache ...
type Cache struct {
	mu   sync.RWMutex
	Twts map[string]*Cached
}

// Store ...
func (cache *Cache) Store(path string) error {
	b := new(bytes.Buffer)
	enc := gob.NewEncoder(b)
	err := enc.Encode(cache)

	if err != nil {
		log.WithError(err).Error("error encoding cache")
		return err
	}

	f, err := os.OpenFile(filepath.Join(path, feedCacheFile), os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.WithError(err).Error("error opening cache file for writing")
		return err
	}

	defer f.Close()

	if _, err = f.Write(b.Bytes()); err != nil {
		log.WithError(err).Error("error writing cache file")
		return err
	}
	return nil
}

// LoadCache ...
func LoadCache(path string) (*Cache, error) {
	cache := &Cache{
		Twts: make(map[string]*Cached),
	}

	f, err := os.Open(filepath.Join(path, feedCacheFile))
	if err != nil {
		if !os.IsNotExist(err) {
			log.WithError(err).Error("error loading cache, cache not found")
			return nil, err
		}
		return cache, nil
	}
	defer f.Close()

	dec := gob.NewDecoder(f)
	err = dec.Decode(&cache)
	if err != nil {
		log.WithError(err).Error("error decoding cache (trying OldCache)")

		f.Seek(0, io.SeekStart)
		oldcache := make(OldCache)
		dec := gob.NewDecoder(f)
		err = dec.Decode(&oldcache)
		if err != nil {
			log.WithError(err).Error("error decoding cache")
			return nil, err
		}
		for url, cached := range oldcache {
			cache.mu.Lock()
			cache.Twts[url] = cached
			cache.mu.Unlock()
		}
	}
	return cache, nil
}

const maxfetchers = 50

// FetchTwts ...
func (cache *Cache) FetchTwts(conf *Config, archive Archiver, feeds types.Feeds, followers map[types.Feed][]string) {
	stime := time.Now()
	defer func() {
		metrics.Gauge(
			"cache",
			"last_processed_seconds",
		).Set(
			float64(time.Now().Sub(stime) / 1e9),
		)
	}()

	// buffered to let goroutines write without blocking before the main thread
	// begins reading
	twtsch := make(chan types.Twts, len(feeds))

	var wg sync.WaitGroup
	// max parallel http fetchers
	var fetchers = make(chan struct{}, maxfetchers)

	metrics.Gauge("cache", "sources").Set(float64(len(feeds)))

	for feed := range feeds {
		wg.Add(1)
		fetchers <- struct{}{}

		// anon func takes needed variables as arg, avoiding capture of iterator variables
		go func(feed types.Feed) {
			defer func() {
				<-fetchers
				wg.Done()
			}()

			headers := make(http.Header)

			if followers != nil {
				feedFollowers := followers[feed]
				if len(feedFollowers) == 1 {
					headers.Set(
						"User-Agent",
						fmt.Sprintf(
							"twtxt/%s (+%s; @%s)",
							twtxt.FullVersion(),
							URLForUser(conf, feedFollowers[0]), feedFollowers[0],
						),
					)
				} else {
					var followersString string

					if len(feedFollowers) > 5 {
						followersString = fmt.Sprintf(
							"%s and %d more... %s",
							strings.Join(feedFollowers[:5], " "),
							(len(feedFollowers) - 5), URLForWhoFollows(conf.BaseURLString(), feed),
						)
					} else {
						followersString = strings.Join(feedFollowers, " ")
					}

					headers.Set(
						"User-Agent",
						fmt.Sprintf(
							"twtxt/%s (Pod: %s Followers: %s Support: %s)",
							twtxt.FullVersion(), conf.Name,
							followersString, URLForPage(conf.BaseURLString(), "support"),
						),
					)
				}
			}

			cache.mu.RLock()
			if cached, ok := cache.Twts[feed.URL]; ok {
				if cached.Lastmodified != "" {
					headers.Set("If-Modified-Since", cached.Lastmodified)
				}
			}
			cache.mu.RUnlock()

			res, err := Request(conf, http.MethodGet, feed.URL, headers)
			if err != nil {
				log.WithError(err).Errorf("error fetching feed %s", feed)
				twtsch <- nil
				return
			}
			defer res.Body.Close()

			actualurl := res.Request.URL.String()
			if actualurl != feed.URL {
				log.WithError(err).Errorf("feed for %s changed from %s to %s", feed.Nick, feed.URL, actualurl)
				feed.URL = actualurl
			}

			if feed.URL == "" {
				log.WithField("feed", feed).Warn("empty url")
				twtsch <- nil
				return
			}

			var twts types.Twts

			switch res.StatusCode {
			case http.StatusOK: // 200
				limitedReader := &io.LimitedReader{R: res.Body, N: conf.MaxFetchLimit}
				scanner := bufio.NewScanner(limitedReader)
				twter := types.Twter{Nick: feed.Nick}
				if strings.HasPrefix(feed.URL, conf.BaseURLString()) {
					twter.URL = URLForUser(conf, feed.Nick)
					twter.Avatar = URLForAvatar(conf, feed.Nick)
				} else {
					twter.URL = feed.URL
					avatar := GetExternalAvatar(conf, feed.Nick, feed.URL)
					if avatar != "" {
						twter.Avatar = URLForExternalAvatar(conf, feed.URL)
					}
				}
				twts, old, err := retwt.ParseFile(scanner, twter, conf.MaxCacheTTL, conf.MaxCacheItems)
				if err != nil {
					log.WithError(err).Errorf("error parsing feed %s", feed)
					twtsch <- nil
					return
				}

				// Archive old twts
				for _, twt := range old {
					if !archive.Has(twt.Hash()) {
						if err := archive.Archive(twt); err != nil {
							log.WithError(err).Errorf("error archiving twt %s aborting", twt.Hash())
							metrics.Counter("archive", "error").Inc()
						} else {
							metrics.Counter("archive", "size").Inc()
						}
					}
				}

				lastmodified := res.Header.Get("Last-Modified")
				cache.mu.Lock()
				cache.Twts[feed.URL] = &Cached{
					cache:        make(map[string]types.Twt),
					Twts:         twts,
					Lastmodified: lastmodified,
				}
				cache.mu.Unlock()
			case http.StatusNotModified: // 304
				cache.mu.RLock()
				twts = cache.Twts[feed.URL].Twts
				cache.mu.RUnlock()
			}

			twtsch <- twts
		}(feed)
	}

	// close twts channel when all goroutines are done
	go func() {
		wg.Wait()
		close(twtsch)
	}()

	for range twtsch {
	}

	cache.mu.RLock()
	metrics.Gauge("cache", "feeds").Set(float64(len(cache.Twts)))
	count := 0
	for _, cached := range cache.Twts {
		count += len(cached.Twts)
	}
	cache.mu.RUnlock()
	metrics.Gauge("cache", "twts").Set(float64(count))
}

// Lookup ...
func (cache *Cache) Lookup(hash string) (types.Twt, bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	for _, cached := range cache.Twts {
		twt, ok := cached.Lookup(hash)
		if ok {
			return twt, true
		}
	}
	return &types.NilTwt{}, false
}

func (cache *Cache) Count() int {
	var count int
	cache.mu.RLock()
	for _, cached := range cache.Twts {
		count += len(cached.Twts)
	}
	cache.mu.RUnlock()
	return count
}

// GetAll ...
func (cache *Cache) GetAll() types.Twts {
	var alltwts types.Twts
	cache.mu.RLock()
	for _, cached := range cache.Twts {
		alltwts = append(alltwts, cached.Twts...)
	}
	cache.mu.RUnlock()
	return alltwts
}

// GetMentions ...
func (cache *Cache) GetMentions(u *User) (twts types.Twts) {
	seen := make(map[string]bool)

	// Search for @mentions in the cache against all Twts (local, followed and even external if any)
	for _, twt := range cache.GetAll() {
		for _, mention := range twt.Mentions() {
			if u.Is(mention.Twter().URL) && !seen[twt.Hash()] {
				twts = append(twts, twt)
				seen[twt.Hash()] = true
			}
		}
	}

	return
}

// GetByPrefix ...
func (cache *Cache) GetByPrefix(prefix string, refresh bool) types.Twts {
	key := fmt.Sprintf("prefix:%s", prefix)
	cache.mu.RLock()
	cached, ok := cache.Twts[key]
	cache.mu.RUnlock()
	if ok && !refresh {
		return cached.Twts
	}

	var twts types.Twts

	cache.mu.RLock()
	for url, cached := range cache.Twts {
		if strings.HasPrefix(url, prefix) {
			twts = append(twts, cached.Twts...)
		}
	}
	cache.mu.RUnlock()

	sort.Sort(twts)

	cache.mu.Lock()
	cache.Twts[key] = &Cached{
		cache:        make(map[string]types.Twt),
		Twts:         twts,
		Lastmodified: time.Now().Format(time.RFC3339),
	}
	cache.mu.Unlock()

	return twts
}

// IsCached ...
func (cache *Cache) IsCached(url string) bool {
	cache.mu.RLock()
	_, ok := cache.Twts[url]
	cache.mu.RUnlock()
	return ok
}

// GetByURL ...
func (cache *Cache) GetByURL(url string) types.Twts {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	if cached, ok := cache.Twts[url]; ok {
		return cached.Twts
	}
	return types.Twts{}
}

// Delete ...
func (cache *Cache) Delete(feeds types.Feeds) {
	for feed := range feeds {
		cache.mu.Lock()
		delete(cache.Twts, feed.URL)
		cache.mu.Unlock()
	}
}
