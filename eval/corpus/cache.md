# Read-through cache

The catalog uses a read-through cache in front of Postgres to serve product pages.
On a cache miss it loads from the database and populates the cache with a sixty
second TTL. Writes invalidate the cache key so stale prices are never served.
