-- 0018_edge_trust — propagate trust from source chunks onto extracted edges (#367).
--
-- #273 tags each chunk's trust and forces untrusted extraction through review, but
-- once an operator approves such an edge it becomes a live "fact" with no marker
-- of its untrusted origin — a recalled edge whose `why` came from a poisoned
-- document looks identical to one a human captured. Carry the trust onto the edge
-- so recall can surface it and a client can weigh the relationship accordingly.
--
-- Expand-only (#261): NOT NULL DEFAULT 'trusted' grandfathers every existing edge
-- (chat-captured edges are trusted); only extractor edges from untrusted chunks
-- are written 'untrusted'.
ALTER TABLE edges
    ADD COLUMN trust text NOT NULL DEFAULT 'trusted'
    CHECK (trust IN ('trusted', 'untrusted'));
