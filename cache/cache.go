package cache

import (
	"time"

	"github.com/Code-Hex/synchro"
	"github.com/Code-Hex/synchro/tz"
)

type CacheStorage[T comparable] interface {
	Set(key string, value T)
	Get(key string) (T, bool)
	Delete(key string)
	BulkSet(data map[string]T)
	BulkGet(keys []string) map[string]T
	BulkDelete(keys []string)
	GetAll(mustIncludeKeys ...string) map[string]T
	DeleteAll()
}

type cacheStorage[T comparable] struct {
	data      map[string]T
	exipresAt synchro.Time[tz.AsiaTokyo]
	exiresIn  time.Duration
}

func NewCacheStorage[T comparable](exiresIn time.Duration) CacheStorage[T] {
	return &cacheStorage[T]{
		data:      make(map[string]T),
		exipresAt: synchro.Now[tz.AsiaTokyo](),
		exiresIn:  exiresIn,
	}
}

func (c *cacheStorage[T]) Set(key string, value T) {
	c.data[key] = value
	c.exipresAt = synchro.Now[tz.AsiaTokyo]().Add(c.exiresIn)
}

func (c *cacheStorage[T]) Get(key string) (T, bool) {
	if synchro.Now[tz.AsiaTokyo]().After(c.exipresAt) {
		c.DeleteAll()
		var v T
		return v, false
	}

	value, ok := c.data[key]
	return value, ok
}

func (c *cacheStorage[T]) Delete(key string) {
	delete(c.data, key)
	c.exipresAt = synchro.Now[tz.AsiaTokyo]().Add(c.exiresIn)
}

func (c *cacheStorage[T]) BulkSet(data map[string]T) {
	for k, v := range data {
		c.data[k] = v
	}
	c.exipresAt = synchro.Now[tz.AsiaTokyo]().Add(c.exiresIn)
}

func (c *cacheStorage[T]) BulkGet(keys []string) map[string]T {
	if synchro.Now[tz.AsiaTokyo]().After(c.exipresAt) {
		c.DeleteAll()
		return make(map[string]T, len(keys))
	}
	data := make(map[string]T)
	for _, k := range keys {
		if v, ok := c.data[k]; ok {
			data[k] = v
		}
	}
	return data
}

func (c *cacheStorage[T]) BulkDelete(keys []string) {
	for _, k := range keys {
		delete(c.data, k)
	}
	c.exipresAt = synchro.Now[tz.AsiaTokyo]().Add(c.exiresIn)
}

func (c *cacheStorage[T]) GetAll(mustIncludeKeys ...string) map[string]T {
	include := true
	if len(mustIncludeKeys) > 0 {
		for _, k := range mustIncludeKeys {
			if _, ok := c.data[k]; !ok {
				include = false
				break
			}
		}
	}

	if synchro.Now[tz.AsiaTokyo]().After(c.exipresAt) || !include {
		c.DeleteAll()
		return make(map[string]T)
	}

	return c.data
}

func (c *cacheStorage[T]) DeleteAll() {
	c.data = make(map[string]T)
	c.exipresAt = synchro.Now[tz.AsiaTokyo]().Add(c.exiresIn)
}
