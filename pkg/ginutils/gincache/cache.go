package gincache

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/trustwallet/blockatlas/pkg/errors"
	"github.com/trustwallet/blockatlas/pkg/logger"
	"github.com/trustwallet/blockatlas/pkg/storage/util"
	"io/ioutil"
	"net/http"
	"sync"
	"time"
)

var (
	memoryCache *memCache
)

func init() {
	memoryCache = &memCache{cache: cache.New(5*time.Minute, 5*time.Minute)}
}

type memCache struct {
	sync.RWMutex
	cache *cache.Cache
}

type cacheResponse struct {
	Status int
	Header http.Header
	Data   []byte
}

type cachedWriter struct {
	gin.ResponseWriter
	status  int
	written bool
	expire  time.Duration
	key     string
}

var _ gin.ResponseWriter = &cachedWriter{}

func newCachedWriter(expire time.Duration, writer gin.ResponseWriter, key string) *cachedWriter {
	return &cachedWriter{writer, 0, false, expire, key}
}

func (w *cachedWriter) WriteHeader(code int) {
	w.status = code
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *cachedWriter) Status() int {
	return w.ResponseWriter.Status()
}

func (w *cachedWriter) Written() bool {
	return w.ResponseWriter.Written()
}

func (w *cachedWriter) Write(data []byte) (int, error) {
	ret, err := w.ResponseWriter.Write(data)
	if err != nil {
		return 0, errors.E(err, "fail to cache write string", errors.Params{"data": data})
	}
	if w.Status() != 200 {
		return 0, errors.E("Write: invalid cache status", errors.Params{"data": data})
	}
	val := cacheResponse{
		w.Status(),
		w.Header(),
		data,
	}
	memoryCache.cache.Set(w.key, val, w.expire)
	return ret, nil
}

func (w *cachedWriter) WriteString(data string) (n int, err error) {
	ret, err := w.ResponseWriter.WriteString(data)
	if err != nil {
		return 0, errors.E(err, "fail to cache write string", errors.Params{"data": data})
	}
	if w.Status() != 200 {
		return 0, errors.E("WriteString: invalid cache status", errors.Params{"data": data})
	}
	val := cacheResponse{
		w.Status(),
		w.Header(),
		[]byte(data),
	}
	memoryCache.setCache(w.key, val, w.expire)
	return ret, err
}

func (mc *memCache) deleteCache(key string) {
	mc.RLock()
	defer mc.RUnlock()
	memoryCache.cache.Delete(key)
}

func (mc *memCache) setCache(k string, x interface{}, d time.Duration) {
	b, err := json.Marshal(x)
	if err != nil {
		logger.Error(errors.E(err, "client cache cannot marshal cache object").PushToSentry())
		return
	}
	mc.RLock()
	defer mc.RUnlock()
	memoryCache.cache.Set(k, b, d)
}

func (mc *memCache) getCache(key string) (*cacheResponse, error) {
	c, ok := mc.cache.Get(key)
	if !ok {
		return nil, fmt.Errorf("gin-cache: invalid cache key %s", key)
	}
	r, ok := c.([]byte)
	if !ok {
		return nil, errors.E("validator cache: failed to cast cache to bytes")
	}
	var result cacheResponse
	err := json.Unmarshal(r, &result)
	if err != nil {
		return nil, errors.E(err, util.ErrNotFound).PushToSentry()
	}
	return &result, nil
}

func generateKey(c *gin.Context) string {
	url := c.Request.URL.String()
	var b []byte
	if c.Request.Body != nil {
		b, _ = ioutil.ReadAll(c.Request.Body)
		// Restore the io.ReadCloser to its original state
		c.Request.Body = ioutil.NopCloser(bytes.NewBuffer(b))
	}
	hash := sha1.Sum(append([]byte(url), b...))
	return base64.URLEncoding.EncodeToString(hash[:])
}

// CacheMiddleware encapsulates a gin handler function and caches the response with an expiration time.
func CacheMiddleware(expiration time.Duration, handle gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer c.Next()
		key := generateKey(c)
		mc, err := memoryCache.getCache(key)
		if err != nil || mc.Data == nil {
			writer := newCachedWriter(expiration, c.Writer, key)
			c.Writer = writer
			handle(c)

			if c.IsAborted() {
				memoryCache.deleteCache(key)
			}
			return
		}

		c.Writer.WriteHeader(mc.Status)
		for k, vals := range mc.Header {
			for _, v := range vals {
				c.Writer.Header().Set(k, v)
			}
		}
		_, err = c.Writer.Write(mc.Data)
		if err != nil {
			memoryCache.deleteCache(key)
			logger.Error(err, "cannot write data", mc)
		}
	}
}
