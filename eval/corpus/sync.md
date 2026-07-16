# Why we rejected synchronous sync

We considered synchronous replication between the primary and the search index but
rejected it: it coupled write latency to index availability, so an index blip stalled
checkout. We chose asynchronous, event-driven sync instead, accepting eventual
consistency in search for much lower write latency and better isolation of failures.
