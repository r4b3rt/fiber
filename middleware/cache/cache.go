// Special thanks to @codemicro for moving this to fiber core
// Original middleware: github.com/codemicro/fiber-cache
package cache

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
)

// timestampUpdatePeriod is the period which is used to check the cache expiration.
// It should not be too long to provide more or less acceptable expiration error, and in the same
// time it should not be too short to avoid overwhelming of the system
const timestampUpdatePeriod = 300 * time.Millisecond

// cache status
// unreachable: when cache is bypass, or invalid
// hit: cache is served
// miss: do not have cache record
const (
	cacheUnreachable = "unreachable"
	cacheHit         = "hit"
	cacheMiss        = "miss"
)

var ignoreHeaders = map[string]interface{}{
	"Connection":          nil,
	"Keep-Alive":          nil,
	"Proxy-Authenticate":  nil,
	"Proxy-Authorization": nil,
	"TE":                  nil,
	"Trailers":            nil,
	"Transfer-Encoding":   nil,
	"Upgrade":             nil,
	"Content-Type":        nil, // already stored explicitely by the cache manager
	"Content-Encoding":    nil, // already stored explicitely by the cache manager
}

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Nothing to cache
	if int(cfg.Expiration.Seconds()) < 0 {
		return func(c *fiber.Ctx) error {
			return c.Next()
		}
	}

	var (
		// Cache settings
		mux       = &sync.RWMutex{}
		timestamp = uint64(time.Now().Unix())
	)
	// Create manager to simplify storage operations ( see manager.go )
	manager := newManager(cfg.Storage)

	// Update timestamp in the configured interval
	go func() {
		for {
			atomic.StoreUint64(&timestamp, uint64(time.Now().Unix()))
			time.Sleep(timestampUpdatePeriod)
		}
	}()

	// Return new handler
	return func(c *fiber.Ctx) error {
		// Only cache GET and HEAD methods
		if c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead {
			c.Set(cfg.CacheHeader, cacheUnreachable)
			return c.Next()
		}

		// Get key from request
		// TODO(allocation optimization): try to minimize the allocation from 2 to 1
		key := cfg.KeyGenerator(c) + "_" + c.Method()

		// Get entry from pool
		e := manager.get(key)

		// Lock entry
		mux.Lock()

		// Get timestamp
		ts := atomic.LoadUint64(&timestamp)

		if e.exp != 0 && ts >= e.exp {
			// Check if entry is expired
			manager.delete(key)
			// External storage saves body data with different key
			if cfg.Storage != nil {
				manager.delete(key + "_body")
			}
		} else if e.exp != 0 {
			// Separate body value to avoid msgp serialization
			// We can store raw bytes with Storage 👍
			if cfg.Storage != nil {
				e.body = manager.getRaw(key + "_body")
			}
			// Set response headers from cache
			c.Response().SetBodyRaw(e.body)
			c.Response().SetStatusCode(e.status)
			c.Response().Header.SetContentTypeBytes(e.ctype)
			if len(e.cencoding) > 0 {
				c.Response().Header.SetBytesV(fiber.HeaderContentEncoding, e.cencoding)
			}
			if e.headers != nil {
				for k, v := range e.headers {
					c.Response().Header.SetBytesV(k, v)
				}
			}
			// Set Cache-Control header if enabled
			if cfg.CacheControl {
				maxAge := strconv.FormatUint(e.exp-ts, 10)
				c.Set(fiber.HeaderCacheControl, "public, max-age="+maxAge)
			}

			c.Set(cfg.CacheHeader, cacheHit)

			mux.Unlock()

			// Return response
			return nil
		}

		// make sure we're not blocking concurrent requests - do unlock
		mux.Unlock()

		// Continue stack, return err to Fiber if exist
		if err := c.Next(); err != nil {
			return err
		}

		// lock entry back and unlock on finish
		mux.Lock()
		defer mux.Unlock()

		// Don't cache response if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			c.Set(cfg.CacheHeader, cacheUnreachable)
			return nil
		}

		// Cache response
		e.body = utils.CopyBytes(c.Response().Body())
		e.status = c.Response().StatusCode()
		e.ctype = utils.CopyBytes(c.Response().Header.ContentType())
		e.cencoding = utils.CopyBytes(c.Response().Header.Peek(fiber.HeaderContentEncoding))

		// Store all response headers
		// (more: https://datatracker.ietf.org/doc/html/rfc2616#section-13.5.1)
		if cfg.StoreResponseHeaders {
			e.headers = make(map[string][]byte)
			c.Response().Header.VisitAll(
				func(key []byte, value []byte) {
					// create real copy
					keyS := string(key)
					if _, ok := ignoreHeaders[keyS]; !ok {
						e.headers[keyS] = utils.CopyBytes(value)
					}
				},
			)
		}

		// default cache expiration
		expiration := uint64(cfg.Expiration.Seconds())
		// Calculate expiration by response header or other setting
		if cfg.ExpirationGenerator != nil {
			expiration = uint64(cfg.ExpirationGenerator(c, &cfg).Seconds())
		}
		e.exp = ts + expiration

		// For external Storage we store raw body separated
		if cfg.Storage != nil {
			manager.setRaw(key+"_body", e.body, cfg.Expiration)
			// avoid body msgp encoding
			e.body = nil
			manager.set(key, e, cfg.Expiration)
			manager.release(e)
		} else {
			// Store entry in memory
			manager.set(key, e, cfg.Expiration)
		}

		c.Set(cfg.CacheHeader, cacheMiss)

		// Finish response
		return nil
	}
}
