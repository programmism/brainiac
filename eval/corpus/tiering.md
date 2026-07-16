# Cold storage tiering

Chunks that are rarely retrieved are demoted to a cold tier that is excluded from
the hot vector index, keeping the in-memory index proportional to the working set.
Cold data is promoted back to hot on demand when it is queried again.
