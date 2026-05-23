---
slug: prediction-goat-search-relevance-and-freshness
type: feat
status: active
created: 2026-05-23
depth: standard
target_repo: mvanhorn/printing-press-library
base_branch: feat/prediction-goat
target_cli: library/payments/prediction-goat
---

# feat(prediction-goat): search relevance, freshness gating, venue scoping, and search learning

## Summary

prediction-goat-pp-cli's discovery commands (`topic`, `compare`, `kalshi-series-search`, the screens) silently return wrong or stale answers in three different ways, all reproduced in a tonight's speedrun session:

1. **Search returns junk**: `topic "oscars best picture 2026"` ranks `KXFUSION` (Nuclear fusion) above the real Oscar series. `topic "bitcoin 100k"` misses `KXBTCMAX100`. `topic "portugal world cup"` returns zero Kalshi hits even though `KXMENWORLDCUP-26-PT` exists with $109K 24h volume.
2. **Sync is starved**: 54k synced Kalshi markets are 99.9% one esports prefix family; named events like `KXMENWORLDCUP-*`, `KXBTC*`, `KXOSCAR*` are absent.
3. **Cache freshness is invisible**: cached prices look identical to live prices in output. A user asking "who wins the basketball game tonight" can be served yesterday's odds with no signal that they're stale.

This plan ships eight fixes in one stacked PR against `feat/prediction-goat`:

- Concurrency: WAL mode and busy_timeout on store open
- Search index: curate `resources_fts.content` to title/subtitle/category/ticker/tags (drop raw-JSON pollution)
- Search ranking: title-boost reranking so exact title hits outrank content-tail matches
- Venue scoping: `--venue`, `--polymarket`, `--kalshi` flags on `topic`, `compare`, screens
- Freshness gating: prices and probabilities always fetched live at read time; cache is index only; data age surfaced
- Sync coverage: series-driven Kalshi markets walk seeded from the already-correct `kalshi_series` table
- Surface gaps: `kalshi events get --with-markets`, implement `kalshi events list --series` (currently advertised in `--help` but unimplemented)
- Compounding learning: `search_learnings` table populated by a fire-and-forget `teach` command that the LLM backgrounds right before emitting the user-facing response. No human-visible noise. Future queries on similar questions get the right ticker boosted automatically — the agent stops paying the "took forever to find Portugal's ticker" tax on every session.

---

## Problem Frame

prediction-goat-pp-cli ships as a read-only cross-venue prediction-market CLI for agents. Its central value claim is "one slim ranked bundle of relevant Polymarket + Kalshi markets per topic." Tonight's debugging proved the central claim is broken on representative queries:

| Query | Real top Kalshi | What `topic` returned | Why |
|---|---|---|---|
| portugal world cup | KXMENWORLDCUP-26-PT | nothing | not synced + ranking |
| bitcoin 100k | KXBTCMAX100 family | BTCETHATH-29DEC31 (Ethereum) | implicit-AND FTS, raw-JSON content |
| oscars best picture 2026 | KXOSCARSCORE family | KXFUSION (Nuclear fusion) | source_agencies URLs in content blob match query tokens |
| fed rate cut june | KXRATECUT family | "government spending cut" event | tokenization + raw-JSON noise |
| nba championship 2026 | KXNBA finals | "NBA team" series | sync gap + ranking |

In every case the right answer was either in the local DB and outranked, or never synced. Direct SQL FTS over `resources_fts` returns the correct rows, so the bug is in what we index and how we rank, not the FTS engine itself.

Separately, the freshness model conflates "this market exists" with "this is the current price." For a question like "what are the odds Lakers win tonight," a 6-hour-old cached row is dangerous because prices move continuously and the question is time-sensitive. The CLI treats both as the same data and offers no signal of age.

## Requirements

Sourced from user request (this session) and from `feedback.jsonl` entries 1-7 logged this session.

- **R1.** `topic "<query>"` must return Polymarket and Kalshi markets whose titles actually concern the query topic, not markets whose raw-JSON metadata happens to contain the query tokens. Verified by the five speedrun cases above.
- **R2.** Concurrent calls to read commands (e.g. parallel `topic` queries from an agent) must not fail with `SQLITE_BUSY`.
- **R3.** Discovery commands (`topic`, `compare`, screens) must accept `--venue polymarket|kalshi|all` and the shortcut flags `--polymarket` / `--kalshi`. Default behavior (no flag) stays "both."
- **R4.** Any output that surfaces a price or implied probability (`yesPercent`, `yesProbability`, `last_price`, `yes_ask_dollars`) must reflect the live upstream value at command time, never a cached value.
- **R5.** Output must surface cache age for the discovery layer (e.g., "Index synced 14h ago") so an agent or user can detect stale-index conditions even when prices are live.
- **R6.** `kalshi sync` must reach named event families (`KXMENWORLDCUP`, `KXBTC*`, `KXOSCAR*`) in default operation, not stop on a single prefix.
- **R7.** `kalshi events get` must support a `--with-markets` flag that returns the event's nested markets in one call.
- **R8.** `kalshi events list --series <ticker>` must work as advertised in `--help` (currently the flag does not exist).
- **R9.** The CLI must learn from the LLM's final response to the user. A `teach` command, designed to be backgrounded by the LLM at response-time, takes the original user question plus the tickers/slugs that appear in the LLM's response and records boost mappings. No user-visible noise: silent on success, errors logged to a local file. The skill markdown instructs the LLM to fire this command in the background before emitting the user-facing answer.
- **R10.** The CLI must expose a `recall` command that the LLM calls FIRST when starting work on a new user question. `recall "<query>"` returns prior learnings matching that query (by token-set overlap, not just exact match) so the agent can short-circuit discovery entirely when confident learnings exist — going straight from question to live price fetch in two CLI calls instead of seven. Returns an empty result when no learnings match.
- **R11.** Learnings must be inspectable, reversible, and disableable — `learnings list` and `forget` commands; `--no-learn` per-invocation flag and `PREDICTION_GOAT_NO_LEARN=true` env var; JSONL audit log alongside the table row so a user can grep history without SQL.

## Scope Boundaries

In scope:
- Search ranking and index composition for the existing `topic`, `compare`, and `kalshi-series-search` commands
- Freshness handling across `topic`, `compare`, `mispriced`, `markets get-by-slug`, and the screens (`trending`, `resolving`, `liquid`, `movers`, `new`)
- Kalshi sync coverage and on-demand event-market fetch
- Venue scoping flags
- New `teach` / `learnings list` / `forget` commands, the skill-side LLM contract that fires `teach` at response-time, and the reranking layer that applies it

Out of scope:
- Trading, order placement, or any write surface against the venue APIs (CLI is read-only by structural CI lint)
- MCP server changes (`prediction-goat-pp-mcp`) — wrappers ride on whatever the CLI exposes
- Polymarket-side sync improvements (Polymarket's `events`/`markets` tables also cap at ~100 rows per sync; separate concern)
- Auto-refresh of the entire local DB before every command — rejected in favor of live-on-read for prices + cache for index

### Deferred to Follow-Up Work

- Polymarket sync coverage parity (its own PR)
- A `kalshi sync --since <duration>` incremental refresh path
- An MCP tool surface for `learnings list` / `forget`
- Cross-machine sync of learnings (shared registry of community-confirmed mappings)

---

## Key Technical Decisions

**Live-on-read for prices, cache for index.** The local SQLite DB is repositioned as a discovery index — "which markets exist, what are they called, what category" — never as a price/probability source. Every command that displays a price refetches the relevant tickers from upstream at command time. This fixes the basketball-tonight risk without forcing a full DB resync before every read.

**Curate `resources_fts.content` to a small, semantic projection.** Currently `store.go:1258` indexes the entire raw JSON blob, which is why KXFUSION (Nuclear fusion) matches "oscars" via `oscars.org/oscars` in its `source_agencies` URL list and matches "picture" via "Academy of Motion Picture Arts and Sciences." The replacement projection is title + subtitle/yes_sub_title + category + ticker + tags + parent event title. This is the single biggest search-quality lever in this plan.

**Schema bump + lazy reindex.** Changing the FTS content projection requires reindexing. Use a `PRAGMA user_version` bump from N to N+1 and trigger a one-shot rebuild of `resources_fts` on first `Open` at the new version. Existing users get a 30-second reindex once; future opens are no-ops.

**Title-boost ranking via UNION + ordered priority.** Rather than overload FTS5's bm25 weighting (which is brittle), run two FTS queries — one against title-only, one against the full curated content — and UNION with a priority column. Title hits rank above content hits at the same FTS score. This is cheap, predictable, and easy to test.

**Series-driven sync walk.** The existing `kalshi_series` corpus (10,363 rows, synced correctly) is the seed list. For each series, fan out one call per series to `/markets?series_ticker=<X>`. This bypasses the natural-ordering problem (where Kalshi's `/markets` endpoint returns 42k KXMVE esports markets before reaching any named-event market) and gives proportional coverage across all series.

**Venue scoping as composable flags.** `--venue polymarket|kalshi|all` is the canonical flag, matching the existing `liquid` command's convention. `--polymarket` and `--kalshi` are boolean shortcuts that desugar to `--venue=polymarket` / `--venue=kalshi`. Default stays "both" so the existing single-call cross-venue value is preserved.

**`search_learnings` is populated by the LLM's act of responding, not by user-typed commands or CLI-side heuristics.** The LLM is the only party that knows (a) the user's original natural-language question and (b) which tickers ended up in the response. So put the signal there. A new `teach` command takes a query string plus one or more resource IDs and writes a boost learning per pair. The skill markdown documents the contract: right before sending the user-facing response, the LLM fires `prediction-goat-pp-cli teach --query "<original question>" --resource <id> [--resource <id>...] &` in the background. `teach` is silent on success (no stdout, errors only to `~/.local/share/prediction-goat-pp-cli/teach.log`), safe to background, and idempotent on (query, resource, action). The reranking layer in `topic` and `compare` reads `search_learnings` to boost / hide / alias hits on subsequent queries. `--no-learn` per-invocation and `PREDICTION_GOAT_NO_LEARN=true` env var disable the apply side for deterministic agent flows. The earlier idea of CLI-side command-sequence correlation (a `last_discovery.json` sidecar) is rejected — it required the LLM to drill into a specific ticker after a discovery query, which is not how agent flows actually work and would have produced both missed learnings (when the LLM answered directly from `topic` output) and false positives (when the LLM drilled into something for an unrelated reason).

---

## Output Structure

New files added under the existing CLI tree:

```
library/payments/prediction-goat/
└── internal/
    ├── cli/
    │   ├── teach.go          # new — teach (LLM-facing, silent, backgroundable) + learnings list + forget
    │   ├── venue.go          # new — venue resolution helper shared by topic/compare/screens
    │   └── freshness.go      # new — live-price fetch + cache age helpers
    ├── store/
    │   └── learnings.go      # new — search_learnings table CRUD + reranking apply
    └── source/
        └── kalshi/
            └── series_walk.go # new — series-seeded markets sync
```

Modified files: `internal/cli/topic.go`, `internal/cli/compare.go`, `internal/cli/liquid.go`, `internal/cli/kalshi.go`, `internal/cli/mispriced.go`, `internal/cli/trending.go`, `internal/cli/movers.go`, `internal/cli/resolving.go`, `internal/cli/new.go`, `internal/store/store.go`, `internal/source/kalshi/sync.go`, `internal/source/kalshi/client.go`.

---

## High-Level Technical Design

### Query lifecycle, before vs. after

```
BEFORE:
topic "portugal world cup"
  -> topicFTSQuery wraps tokens: "portugal" "world" "cup"  (implicit AND)
  -> SELECT ... FROM resources_fts WHERE content MATCH ?  (content = raw JSON)
  -> ORDER BY bm25 rank
  -> return cached prices

AFTER:
topic "portugal world cup"
  -> rerank-layer pre-pass: any pinned mappings for "portugal world cup"? (learn table)
  -> topicFTSQuery -> two FTS queries (title-only + curated-content)
  -> UNION with priority column (title=1, content=2), then bm25
  -> for any hit that carries a price: refetch from live API in parallel
  -> rerank-layer post-pass: apply hide/alias rules
  -> output includes meta.synced_at + meta.price_source=live
```

*Directional guidance for review, not implementation specification.*

### Freshness boundary

```
+------------------+-----------------+----------------------+
| Field            | Source          | Acceptable staleness |
+------------------+-----------------+----------------------+
| ticker / slug    | local cache     | days (it's an ID)    |
| title, category  | local cache     | hours (rarely change)|
| end_date         | local cache     | hours                |
| yesProbability   | LIVE API ONLY   | <60s (current price) |
| yes_ask_dollars  | LIVE API ONLY   | <60s                 |
| volume_24h       | LIVE API ONLY   | <60s                 |
| status           | LIVE API ONLY   | <60s                 |
+------------------+-----------------+----------------------+
```

*Directional guidance for review, not implementation specification.*

### Learning pipeline (LLM-driven, two-sided)

**Cold start (no prior learnings for this question family):**

```
human: "what are the odds Portugal wins the world cup?"
   |
   v
LLM: $ prediction-goat-pp-cli recall "portugal world cup odds" --agent
   <- { "found": false, "results": [] }                  # nothing cached
   |
   v
LLM: $ topic "portugal world cup" --agent
LLM: $ compare "portugal world cup" --agent
LLM: $ kalshi-series-search WORLDCUP ...
LLM: $ kalshi markets get KXMENWORLDCUP-26-PT --agent   # finally lands on the right ticker
LLM: $ markets get-by-slug will-portugal-...            # and the PM side
   |
   v (BEFORE emitting the user-facing message)
LLM: $ prediction-goat-pp-cli teach \
        --query "portugal world cup odds" \
        --resource KXMENWORLDCUP-26-PT \
        --resource will-portugal-win-the-2026-fifa-world-cup-912 \
        --quiet &                                        # fire-and-forget; silent
   |
   v
LLM emits the % answer. Human sees: "Portugal 8.8% / 8.5%." Nothing else.
```

**Warm start (same question family, days later):**

```
human: "portugal's chances at the world cup?"
   |
   v
LLM: $ prediction-goat-pp-cli recall "portugal chances world cup" --agent
   <- {
        "found": true,
        "match_score": 0.82,
        "results": [
          { "resource_id": "KXMENWORLDCUP-26-PT",                   "venue": "kalshi",     "confidence": 3 },
          { "resource_id": "will-portugal-win-the-2026-fifa...",    "venue": "polymarket", "confidence": 3 }
        ]
      }
   |
   v
LLM: $ kalshi markets get KXMENWORLDCUP-26-PT --agent           # straight to live price
LLM: $ markets get-by-slug will-portugal-... --agent            # straight to live price
   |
   v
LLM: $ teach --query "portugal chances world cup" --resource ... --resource ... &
   |
   v
LLM emits the % answer. Two discovery calls, not seven. Human sees the same clean output.
```

Skill markdown (`pp-prediction-goat/SKILL.md`) documents both sides of this contract — `recall` before discovery, `teach` after response. The CLI's `topic` / `compare` commands ALSO apply the rerank layer internally as belt-and-suspenders: a forgetful LLM that skips `recall` still benefits from the learnings, just less efficiently.

### `search_learnings` table shape

```sql
CREATE TABLE search_learnings (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  query_pattern TEXT NOT NULL,        -- normalized lowercase, whitespace-collapsed
  venue TEXT,                         -- 'polymarket', 'kalshi', NULL = both
  resource_type TEXT,                 -- 'markets', 'kalshi_markets', ...
  resource_id TEXT NOT NULL,          -- slug or ticker
  action TEXT NOT NULL,               -- 'boost' | 'hide' | 'alias_of'
  alias_target TEXT,                  -- non-null iff action='alias_of'
  source TEXT NOT NULL,               -- 'taught' (from LLM teach call) | 'manual-forget'
  confidence INTEGER DEFAULT 1,       -- increments on re-observation
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  last_observed_at DATETIME,
  notes TEXT
);
CREATE INDEX idx_learn_query ON search_learnings(query_pattern);
CREATE UNIQUE INDEX idx_learn_unique ON search_learnings(query_pattern, resource_id, action);
```

*Directional guidance for review, not implementation specification.*

---

## Implementation Units

### U1. Enable WAL mode and busy_timeout on store open

**Goal:** Eliminate `SQLITE_BUSY` failures from concurrent agent reads and from sync-vs-read overlap.

**Requirements:** R2.

**Dependencies:** none — foundation for everything else.

**Files:**
- `library/payments/prediction-goat/internal/store/store.go` (modify `OpenWithContext` and `OpenWithDeadline`)
- `library/payments/prediction-goat/internal/store/store_concurrency_test.go` (new)

**Approach:** On open, issue `PRAGMA journal_mode=WAL` and `PRAGMA busy_timeout=5000` against the connection. Both pragmas are session-level, so they need to be applied after each `sql.Open` and on any connection retrieved via `Conn()`. Verify journal mode came back as `wal` (some filesystems refuse WAL — fall back to delete journal with a stderr note).

**Patterns to follow:** Look at existing `OpenWithContext` for the connection-init sequence and emulate the existing migration-lock retry helper for any failure cases.

**Test scenarios:**
- Two `topic` commands launched concurrently both succeed.
- A `kalshi sync` and a `topic` command launched concurrently both succeed.
- `journal_mode` reads back as `wal` on a tmpfile-backed DB.
- A filesystem that rejects WAL falls back cleanly to delete journal and logs a single warning to stderr (skip on CI where this can't be reliably faked).

**Verification:** `go test ./internal/store/... -run Concurrency` passes; running `prediction-goat-pp-cli topic foo` in two terminals while a sync is mid-flight produces no `database is locked` errors.

---

### U2. Curate `resources_fts.content` to a semantic projection

**Goal:** Stop indexing raw JSON. Index only fields that carry topical signal so KXFUSION can no longer match "oscars best picture."

**Requirements:** R1.

**Dependencies:** U1 (reindex runs inside a write transaction; WAL avoids blocking concurrent readers).

**Execution note:** Characterization-first — write a failing test that asserts `KXFUSION` does NOT match the query `oscars best picture 2026` and `KXOSCARSCORE` DOES match it. The current indexer fails this test.

**Files:**
- `library/payments/prediction-goat/internal/store/store.go` (modify `resourcesFTSCreateSQL`, the insert at line ~1258, and the rebuild path)
- `library/payments/prediction-goat/internal/store/fts_content_test.go` (new)

**Approach:** Replace the raw-JSON `content` column with a derived projection computed at upsert time. The projection per resource type:

- `markets` (Polymarket): `question + ' ' + title + ' ' + slug + ' ' + category`
- `events` (Polymarket): `title + ' ' + slug + ' ' + category`
- `tags`: `label + ' ' + slug`
- `kalshi_markets`: `title + ' ' + yes_sub_title + ' ' + ticker + ' ' + event_ticker + ' ' + category`
- `kalshi_events`: `title + ' ' + event_ticker + ' ' + category + ' ' + series_ticker`
- `kalshi_series`: `title + ' ' + ticker + ' ' + category`

Bump `PRAGMA user_version` from current N to N+1 and trigger a one-shot rebuild of `resources_fts` on Open at the new version. Drop and recreate the FTS table inside the migration-lock helper to avoid races.

**Patterns to follow:** The existing migration sequence already drops and recreates `resources_fts` in one of the older migrations (search for `DROP TABLE IF EXISTS resources_fts` around line 1184). Mirror that pattern.

**Test scenarios:**
- KXFUSION (Nuclear fusion series) is NOT returned by FTS MATCH `oscars` after reindex.
- KXOSCARCOUNTCONCLAVE IS returned by FTS MATCH `oscars` after reindex.
- KXBTCMAX100 IS returned by FTS MATCH `bitcoin`.
- KXMENWORLDCUP-26-PT (synthetic fixture row) IS returned by FTS MATCH `portugal world cup`.
- Schema version bumps from N to N+1 on first open; subsequent opens are no-ops.
- Reindexing a 10k-row corpus completes in under 30s on a developer laptop.

**Verification:** `go test ./internal/store/... -run FTSContent` passes; running `prediction-goat-pp-cli topic "oscars best picture 2026"` returns Oscar-related Kalshi hits, not KXFUSION.

---

### U3. Title-boost reranking in `topicFTSQuery`

**Goal:** Hits where the query token appears in the title outrank hits where it only appears in deeper content fields.

**Requirements:** R1.

**Dependencies:** U2 (need the curated content column first; title boost is meaningful only when content is curated).

**Files:**
- `library/payments/prediction-goat/internal/cli/topic.go` (modify `topicSearchByTypes`)
- `library/payments/prediction-goat/internal/cli/compare.go` (modify `loadCompareMarkets`)
- `library/payments/prediction-goat/internal/cli/liquid.go` (keep `topicFTSQuery` but extend with a `topicTitleQuery` companion)
- `library/payments/prediction-goat/internal/cli/topic_rank_test.go` (new)

**Approach:** Add a sibling FTS column to `resources_fts` for title-only (`title_only`), populated alongside `content`. Replace the single FTS query with a UNION of two queries with an explicit priority column, then ORDER BY priority ASC, bm25 ASC. The Go layer composes the SQL; no new abstractions.

**Patterns to follow:** Existing FTS query in `topicSearchByTypes` is the shape to extend, not rewrite. Keep `topicFTSQuery` as the token-quoter; add a new helper for the title-only variant if needed.

**Test scenarios:**
- For `bitcoin 100k`, KXBTCMAX100 ranks above any series that contains "bitcoin" only in description/category.
- For `portugal world cup`, KXMENWORLDCUP-26-PT (when present) ranks above any series that contains "portugal" only as a country tag.
- Polymarket and Kalshi hits interleave round-robin (existing behavior preserved).
- Empty result set still produces a `notFoundErr` (existing behavior preserved).

**Verification:** `go test ./internal/cli/... -run TopicRank` passes; the five speedrun queries from Problem Frame all surface the correct top Kalshi hit.

---

### U4. Venue scoping flags on discovery commands

**Goal:** Let an agent skip the second venue when it already knows which platform to hit.

**Requirements:** R3.

**Dependencies:** none — orthogonal to ranking changes.

**Files:**
- `library/payments/prediction-goat/internal/cli/venue.go` (new — `resolveVenue(cmd) string` helper)
- `library/payments/prediction-goat/internal/cli/topic.go` (wire venue resolver, gate poly/kalshi branches)
- `library/payments/prediction-goat/internal/cli/compare.go` (wire venue resolver; `--polymarket` alone is a no-op for compare since compare requires both — error with a helpful message)
- `library/payments/prediction-goat/internal/cli/trending.go`, `movers.go`, `resolving.go`, `new.go` (replace ad-hoc `--venue` parsing with shared resolver)
- `library/payments/prediction-goat/internal/cli/venue_test.go` (new)

**Approach:** Single helper `resolveVenue(cmd) (string, error)` reads `--venue`, `--polymarket`, `--kalshi` from the command's flags and returns one of `"all" | "polymarket" | "kalshi"`. Conflicting flags (`--polymarket --kalshi`, `--polymarket --venue=kalshi`) return a usage error with the conflict named. Default is `"all"`. Each consuming command adds the three flags via a `addVenueFlags(cmd)` helper to keep the surface uniform.

**Patterns to follow:** The existing `liquid` command's `--venue` string flag is the model. Don't rip it out — extend `liquid` to use the shared resolver too.

**Test scenarios:**
- `topic foo --polymarket` returns only Polymarket-source hits.
- `topic foo --kalshi` returns only Kalshi-source hits.
- `topic foo` (no flag) returns both venues interleaved (existing behavior).
- `topic foo --polymarket --kalshi` returns a usage error naming the conflict.
- `topic foo --polymarket --venue=kalshi` returns a usage error naming the conflict.
- `compare foo --polymarket` returns a usage error explaining compare requires both venues.

**Verification:** `go test ./internal/cli/... -run Venue` passes; `prediction-goat-pp-cli topic "world cup" --kalshi --agent | jq '.hits[].source' | sort -u` returns only `"kalshi"`.

---

### U5. Live-on-read freshness for prices and probabilities

**Goal:** Any price or probability returned by the CLI is fetched live at command time, never served from cache. Cache age for the discovery layer is surfaced in `meta`.

**Requirements:** R4, R5.

**Dependencies:** U3 (reranked candidates are the input to the price-refresh step).

**Execution note:** Test-first — write the assertion that a stale `yesProbability` in the cache is overwritten by the live API value before the test passes.

**Files:**
- `library/payments/prediction-goat/internal/cli/freshness.go` (new — `refreshPrices(ctx, hits) []hit` helper for both venues)
- `library/payments/prediction-goat/internal/cli/topic.go` (call refresher before output)
- `library/payments/prediction-goat/internal/cli/compare.go` (same)
- `library/payments/prediction-goat/internal/cli/mispriced.go` (same)
- `library/payments/prediction-goat/internal/cli/trending.go`, `movers.go`, `resolving.go`, `liquid.go`, `new.go` (same)
- `library/payments/prediction-goat/internal/cli/agent_context.go` (extend `meta` envelope with `price_source: "live"` and `index_synced_at` keys)
- `library/payments/prediction-goat/internal/cli/freshness_test.go` (new)

**Approach:** After ranking produces N hits, group hits by venue and issue one batched API call per venue to refresh the price-bearing fields (`yesProbability`, `yes_ask_dollars`, `volume_24h_fp`, `last_price`, `status`). Polymarket's `/markets?slugs=` and Kalshi's `/markets?tickers=` both accept comma-separated lists. Replace the cached values on the in-memory hits before serialization. If a live fetch fails, mark `price_source: "stale"` on those rows and continue rather than failing the whole command — the user still gets the index answer with an explicit staleness flag.

Surface cache age as `meta.index_synced_at` and `meta.price_source` in the response envelope. The existing envelope already carries `meta`, so this is an additive extension. Human-mode (terminal stdout) prints a one-line "Index synced 14h ago, prices live" footer.

**Patterns to follow:** The existing `meta` envelope in `agent_context.go` is the model — extend its keys rather than inventing a new envelope.

**Test scenarios:**
- A cached `markets` row with `lastTradePrice=0.05` returns `yesProbability` matching the live `/markets?slug=` value when topic queries it (use a httptest fixture with a different value).
- A cached `kalshi_markets` row with `yes_ask_dollars=0.085` similarly returns the live value.
- Live fetch failure for one venue degrades that venue's rows to `price_source: "stale"` and leaves the other venue's rows untouched.
- `meta.index_synced_at` is populated from the most recent `sync_state.last_synced_at`.
- Human-mode terminal output includes the cache-age footer; `--json` / `--agent` mode does not (envelope only).
- The basketball-tonight regression test: a synthetic cached row with `yesProbability=0.30` is returned as the live value (e.g., 0.55) after refresh.

**Verification:** `go test ./internal/cli/... -run Freshness` passes; running `prediction-goat-pp-cli topic "lakers" --agent | jq '.meta.price_source'` returns `"live"`.

---

### U6. Series-driven Kalshi markets sync walk

**Goal:** Kalshi sync covers named event families (`KXMENWORLDCUP-*`, `KXBTC*`, `KXOSCAR*`), not just the high-volume esports prefix that currently dominates pagination.

**Requirements:** R6.

**Dependencies:** U1 (WAL mode prevents reindex-vs-sync contention).

**Files:**
- `library/payments/prediction-goat/internal/source/kalshi/series_walk.go` (new)
- `library/payments/prediction-goat/internal/source/kalshi/sync.go` (modify `SyncMarkets` to invoke walk)
- `library/payments/prediction-goat/internal/source/kalshi/client.go` (add `GetMarketsBySeries(ctx, ticker)` if not present)
- `library/payments/prediction-goat/internal/source/kalshi/series_walk_test.go` (new)

**Approach:** After `SyncSeries` populates `kalshi_series` (~10k rows), iterate the rows and call `/markets?series_ticker=<X>&status=open` for each. The fan-out is bounded by `kalshi-series-count` × `pages-per-series`. Use a bounded worker pool (8 workers) and respect the existing `--rate-limit` flag. Persist progress to `sync_state` keyed by series ticker so a re-run resumes mid-walk.

Keep the original `/markets?status=open` pass as a first-pass quick-win (still useful for high-frequency markets), then run the series walk as a second pass. Total wall-clock should remain under 5 minutes for a full sync against Kalshi's current ~10k series.

**Patterns to follow:** Existing `syncPages` cursor loop in `sync.go:57` is the model. Add a `syncPerSeries` companion.

**Test scenarios:**
- After a full sync, `kalshi_markets` contains at least one row with ticker `KXMENWORLDCUP-26-PT`.
- After a full sync, `kalshi_markets` contains rows under the `KXBTC*` and `KXOSCAR*` prefixes.
- Resume mid-walk: kill the sync after N series, restart; resume picks up at series N+1.
- Rate limit: with `--rate-limit 5`, the walk respects 5 req/s.
- A series with zero open markets does not error; it logs and moves on.

**Verification:** `go test ./internal/source/kalshi/... -run SeriesWalk` passes (with httptest fixtures); running `prediction-goat-pp-cli kalshi sync --max-pages 0` followed by `sqlite3 ... "SELECT 1 FROM resources WHERE id='KXMENWORLDCUP-26-PT'"` returns 1.

---

### U7. `kalshi events get --with-markets` and `kalshi events list --series`

**Goal:** Honor advertised behavior; let agents enumerate event-child markets without raw curl.

**Requirements:** R7, R8.

**Dependencies:** none.

**Files:**
- `library/payments/prediction-goat/internal/cli/kalshi.go` (modify `newKalshiEventsGetCmd`, `newKalshiEventsListCmd`)
- `library/payments/prediction-goat/internal/source/kalshi/client.go` (extend `GetEvent` to accept a `WithMarkets bool` option)
- `library/payments/prediction-goat/internal/cli/kalshi_test.go` (extend)

**Approach:** `kalshi events get <ticker> --with-markets` adds `?with_nested_markets=true` to the upstream call and unpacks the nested markets array into the response. `kalshi events list --series <ticker>` issues `/events?series_ticker=<X>` and returns the list. Both are thin pass-throughs.

**Patterns to follow:** Existing `kalshi events get` command in `kalshi.go` is the model. The Kalshi events endpoint already supports both query parameters; only the Go-side flag wiring is missing.

**Test scenarios:**
- `kalshi events get KXMENWORLDCUP-26 --with-markets --agent` returns a payload whose `markets` array length is >= 40.
- `kalshi events list --series KXMENWORLDCUP --agent` returns at least one event (`KXMENWORLDCUP-26`).
- `kalshi events get KXMENWORLDCUP-26` (no flag) returns the existing metadata-only payload (no regression).
- `kalshi events list --series UNKNOWN-SERIES` returns an empty list, not an error.

**Verification:** `go test ./internal/cli/... -run KalshiEvents` passes; `prediction-goat-pp-cli kalshi events get KXMENWORLDCUP-26 --with-markets --agent | jq '.markets | length'` is >= 40.

---

### U8. `recall` + `teach` commands + reranking layer (LLM-driven learning, zero user noise)

**Goal:** The LLM gets two new tools — `recall` to check "do I already know the answer?" before fiddling, and `teach` (backgrounded at response-time) to record what worked. Future queries on the same question family get answered in 2 CLI calls instead of 7. The human user never sees learning chatter.

**Requirements:** R9, R10, R11.

**Dependencies:** U3 (reranking layer sits on top of title-boosted FTS results).

**Files:**
- `library/payments/prediction-goat/internal/store/learnings.go` (new — table CRUD: `Upsert`, `List`, `Forget`, `Apply`)
- `library/payments/prediction-goat/internal/store/store.go` (add `search_learnings` migration in the same schema-version bump as U2)
- `library/payments/prediction-goat/internal/cli/teach.go` (new — `teach`, `recall`, `learnings list`, `forget` subcommands)
- `library/payments/prediction-goat/internal/cli/root.go` (add `--no-learn` persistent flag)
- `library/payments/prediction-goat/internal/cli/topic.go` (call `Apply` on FTS hits; emit `meta.teach_hint` in the envelope)
- `library/payments/prediction-goat/internal/cli/compare.go` (same)
- `library/payments/prediction-goat/internal/cli/agent_context.go` (extend `meta` envelope with `learnings_applied: N` and `teach_hint: "..."` keys)
- `cli-skills/pp-prediction-goat/SKILL.md` **OR** the equivalent skill source location in this repo (TBD — locate at implementation; the published skill at `~/.claude/skills/pp-prediction-goat/SKILL.md` is the installed copy). Add an "Automatic learning" section that documents the LLM contract: fire `teach` in the background before the user-facing response. Wording must be tight — too many words and the LLM stops following it.
- `library/payments/prediction-goat/internal/cli/teach_test.go` (new)
- `library/payments/prediction-goat/internal/store/learnings_test.go` (new)

**Approach:**

**Schema.** See High-Level Technical Design — single `search_learnings` table plus a `(query_pattern, resource_id, action)` unique index so the same `teach` call re-fired bumps `confidence` instead of duplicating rows. Normalize `query_pattern` at write and apply: lowercase, collapse whitespace, strip a small stopword set (`a`, `the`, `what`, `are`, `is`, `was`, `?`, `!`) so `"What are the odds Portugal wins the World Cup?"` and `"portugal world cup odds"` collapse to the same key.

**`teach` command surface (LLM-facing, designed to be backgrounded):**

```text
prediction-goat-pp-cli teach \
  --query "<user's original natural-language question>" \
  --resource <ticker-or-slug> [--resource ...] \
  [--venue polymarket|kalshi] \
  [--quiet]   # default; silent on success
```

Behavior:
- Writes one `boost` row per `--resource`, all under the same normalized `query_pattern`. `source="taught"`. Upsert (no duplicates).
- Exits 0 silently on success — zero stdout, zero stderr. Safe to background with `&` and discard.
- On error (bad DB, full disk, malformed args), writes a single line to `~/.local/share/prediction-goat-pp-cli/teach.log` and exits non-zero. Never prints to stderr. The LLM's background fork can't pollute the user-facing response.
- Idempotent. Calling `teach` twice with identical args bumps confidence; no warning.
- Honors `PREDICTION_GOAT_NO_LEARN=true` — no-op if set.

**`recall` command surface (LLM-facing, the read side of the loop):**

```text
prediction-goat-pp-cli recall "<user's natural-language query>" \
  [--min-confidence N]   # default 1; raise to 2+ to require re-confirmed mappings
  [--limit N]            # default 10
  [--agent]              # JSON envelope
```

Behavior:
- Normalize the query the same way `teach` normalizes (lowercase, collapse whitespace, strip stopwords).
- Match learnings whose normalized `query_pattern` overlaps with the input by token-set Jaccard >= 0.6 (configurable). This catches "portugal world cup chances" → "portugal world cup odds" even though they're not substring-equal.
- Return matching learnings ordered by `(match_score DESC, confidence DESC, last_observed_at DESC)`.
- Each row carries `{resource_id, resource_type, venue, action, confidence, match_score, source, last_observed_at}`.
- When no match: `{"found": false, "results": [], "query": "...", "normalized": "..."}` (still exit 0; not a not-found error — it's an information query).
- When match: `{"found": true, "match_score": <best>, "results": [...]}`.
- Cheap: single SQL pass against a small per-user table; no FTS, no API.

**Skill markdown contract (LLM-facing, short — both sides):**

A new "Automatic learning" section in the prediction-goat skill markdown. Approximate length: 8-10 lines, two paragraphs. Content shape:

> **Before** running `topic` / `compare` / any discovery command for a new user question, run:
>
> `prediction-goat-pp-cli recall "<user's question>" --agent`
>
> If `found=true` with `confidence>=2`, skip discovery and go straight to the live price fetch for the returned tickers (`kalshi markets get <ticker>` / `markets get-by-slug <slug>`). If `found=false`, proceed with normal discovery.
>
> **After** you have finalized your response containing tickers/slugs but BEFORE emitting it to the user, fire this in the background (single line, ampersand at the end):
>
> `prediction-goat-pp-cli teach --query "<user's question>" --resource <id1> --resource <id2> &`
>
> Do not mention either of these to the user. Both are silent on success.

(The exact wording is finalized at implementation time. Optimize for "LLM follows it" rather than completeness.)

**`meta.teach_hint` in response envelope.** As belt-and-suspenders against a forgetful LLM: when `topic` or `compare` returns hits and learnings are not already maxed out for the current normalized query (e.g., the top hit doesn't already have `confidence >= 3` for this query), include a `meta.teach_hint` field in the response envelope:

```json
{
  "meta": {
    "synced_at": "...",
    "price_source": "live",
    "learnings_applied": 0,
    "teach_hint": "If you use these results in your final response, run: prediction-goat-pp-cli teach --query \"<user's original question>\" --resource <id> &"
  },
  "results": [...]
}
```

The LLM sees this in the structured tool output at exactly the moment it has the context. Skill instruction + envelope hint = two reinforcing signals.

**Inspection / undo commands:**

- `learnings list [--query <q>] [--source <s>] [--agent]` — lists rows; supports `--select`. Default sort: `last_observed_at DESC`. Useful for "why did the CLI return this answer?" Different from `recall` in that `list` is for humans inspecting the table, `recall` is for agents looking up by query.
- `forget <query> [--resource <id>] [--action <a>] [--all]` — deletes matching rows; requires at least one filter or `--all`.

The complete learning surface: `recall` (agent reads, before discovery), `teach` (agent writes, after response), `learnings list` (human inspects), `forget` (human undoes). No `learn` command — manual seeding happens via `teach` from the command line if someone really wants it, but that's an LLM-shaped surface, not a human one.

**Reranking layer** (called from `topic` and `compare` after FTS, before envelope assembly):

1. Normalize the current query string the same way `teach` normalizes.
2. Load learnings whose normalized `query_pattern` equals the current query OR is a substring of the current query (cheap LIKE scan; table stays small per-user).
3. For each `boost` rule whose `resource_id` IS among the current FTS hits, move that hit to position 0 (or position N for the Nth boost, ordered by confidence DESC then `last_observed_at` DESC).
4. For each `boost` rule whose `resource_id` is NOT in the FTS hits, fetch the resource from the `resources` table and insert as a synthetic hit at the top with `meta.source: "learned"`. This is the killer mode: FTS missed it entirely, the learning fills the gap.
5. For each `hide` rule, remove matching hits from the result.
6. For each `alias_of` rule, replace hits matching the alias source with the alias target.
7. Detect alias cycles (A → B → A) and drop the cycling rule from this apply with a warning to `teach.log`.
8. Append `meta.learnings_applied: N` to the envelope.

**Audit log.** Each `teach` invocation appends one JSONL line to `~/.local/share/prediction-goat-pp-cli/learnings.jsonl`: `{ts, action: "teach", query, resources, confidence_before_max, confidence_after_max}`. Each `forget` invocation appends one line: `{ts, action: "forget", query, filter, rows_deleted}`.

**Patterns to follow:**
- `feedback.jsonl` (already in CLI) — single JSONL file beside the DB.
- Existing subcommand registration in `internal/cli/root.go`.
- Existing `Upsert` shape in `store/store.go`.
- Existing `meta` envelope in `agent_context.go` — extend keys, don't invent a new envelope.

**Test scenarios:**

*`teach` command:*
- `teach --query "what are Portugal's odds" --resource KXMENWORLDCUP-26-PT` writes one row, exits 0 with zero stdout/stderr.
- Backgrounded form (`teach ... &`) does not block the parent shell or leak output.
- Same call fired twice: confidence increments from 1 → 2, no duplicate row (unique constraint on `(query_pattern, resource_id, action)`).
- Multiple `--resource` flags in one call: one row per resource, all under the same `query_pattern`.
- Normalization: `--query "What are the ODDS Portugal wins?"` and `--query "portugal odds"` collapse to the same row.
- `PREDICTION_GOAT_NO_LEARN=true`: command is a no-op (exits 0, no row written).
- Malformed args (missing `--query`, missing `--resource`): exits non-zero, error to `teach.log`, nothing to stderr.

*`recall` command:*
- After `teach --query "portugal world cup odds" --resource KXMENWORLDCUP-26-PT`, `recall "portugal world cup odds" --agent` returns `found=true` with that ticker at position 0.
- Token overlap match: `recall "portugal chances at the world cup" --agent` matches the learning above (Jaccard overlap >= 0.6 between normalized token sets).
- Unrelated query: `recall "lakers tonight" --agent` returns `found=false`, exits 0 (information query, not error).
- `--min-confidence 2`: filters out confidence-1 learnings.
- `--limit 5`: caps results at 5 even if more match.
- Multiple matching learnings ordered by `match_score DESC, confidence DESC, last_observed_at DESC`.
- Normalization at recall: a learning taught with `"Portugal World Cup"` is returned by `recall "portugal  world cup"` (case + whitespace).
- Stopword stripping symmetry: a learning taught with `"what are portugal's odds"` matches `recall "portugal odds"` (both normalize to `portugal odds`).
- `PREDICTION_GOAT_NO_LEARN=true`: recall returns `{found: false, results: []}` even if learnings exist (apply side is disabled).

*Reranking layer:*
- A `boost` rule whose `resource_id` IS in the current FTS hits moves it to position 0.
- A `boost` rule whose `resource_id` is NOT in the current FTS hits inserts it as a synthetic hit at position 0 with `meta.source: "learned"` — this is the "FTS missed it, learning saved us" case.
- A `hide` rule removes matching hits from the result.
- An `alias_of` rule replaces the alias source with the alias target (after fetching it from `resources`).
- Substring match: a rule on `query_pattern="bitcoin"` applies to query `bitcoin 100k`.
- Normalization at apply: a rule recorded for `"Portugal World Cup"` applies to query `portugal  world cup` (case + whitespace + stopwords).
- Alias cycle A → B → A is detected and dropped from this apply with a warning to `teach.log`.
- `--no-learn` on a `topic` / `compare` invocation: rerank layer is skipped (FTS-only output).

*Envelope:*
- `meta.learnings_applied` reflects the count of rules that touched the output (0 when none applied).
- `meta.teach_hint` is present when the top hit has low confidence for the current normalized query, absent when it has high confidence (>= 3).

*Inspection / undo:*
- `learnings list --agent` returns all rows as JSON, supports `--select`.
- `learnings list --query "portugal"` filters to substring matches.
- `forget "portugal world cup" --resource KXMENWORLDCUP-26-PT` removes the boost; next `topic` reverts to FTS ranking for that query.
- `forget "portugal world cup" --all` wipes every rule for that query.
- The JSONL audit log gets one line per `teach` and per `forget`.

**Verification:** `go test ./internal/cli/... -run "Teach|Recall"` and `go test ./internal/store/... -run Learnings` pass. End-to-end manual check, simulating the Portugal flow from tonight's session:

1. `recall "portugal world cup odds" --agent` → `found=false` (cold start).
2. `topic "portugal world cup"` → no Portugal in results (the bug).
3. `kalshi markets get KXMENWORLDCUP-26-PT --agent` → Portugal 8.5%.
4. `teach --query "portugal world cup odds" --resource KXMENWORLDCUP-26-PT --resource will-portugal-win-the-2026-fifa-world-cup-912` → silent exit 0.
5. `recall "portugal world cup odds" --agent` → `found=true`, both tickers at positions 0 and 1 with confidence 1.
6. `recall "portugal chances at the world cup" --agent` → `found=true` (token overlap >= 0.6).
7. `topic "portugal world cup"` → both tickers at positions 0 and 1 (rerank applied internally).

---

## System-Wide Impact

- **Schema migration:** U2 and U8 both ride a single `PRAGMA user_version` bump. First open at the new version triggers reindex of `resources_fts` and creation of `search_learnings`. Document in the release note: existing users get a 30-second one-time reindex.
- **Live API call volume:** U5 changes the model from "0 API calls per topic/compare invocation" to "1-2 batched API calls per invocation." For an agent running `topic` 20× per hour, this is 20-40 extra calls per hour against publicly free endpoints — within rate-limit budget for both venues.
- **Sync wall-clock:** U6 adds a series-walk pass to `kalshi sync`. Full sync goes from ~2 minutes to ~5 minutes against current Kalshi corpus.
- **MCP server:** No changes required. `prediction-goat-pp-mcp` wraps the CLI; new commands (`learn`, `forget`, `learnings list`) are not exposed via MCP in this PR — deferred.
- **CI lint:** The read-only structural lint is unaffected. The new `learn`/`forget` commands write only to the local SQLite DB and JSONL log; no trading API surface is introduced.

---

## Risks and Mitigations

- **Reindex during heavy session lags command latency.** U2's one-shot reindex takes ~30s on a 60k-row corpus. Mitigation: WAL mode (U1) means the reindex doesn't block reads — concurrent `topic` queries during reindex return FTS hits from the pre-migration index, slightly stale but available. Document the one-time hit in release notes.
- **Live API rate limits.** Polymarket and Kalshi both impose rate limits. U5's batched fetches use comma-separated ticker lists (one call per venue per invocation), keeping us within budget. If a future change fans out per-ticker, revisit.
- **Series-walk explosion.** U6 fan-outs across 10k series. Bounded worker pool (8 workers) plus `--rate-limit` keeps this safe. Worst case: full sync takes 10 minutes instead of 5.
- **Learnings drift.** A user can pollute `search_learnings` with bad rules. Mitigations: `learnings list` surfaces all rules; `forget` is fast; the JSONL audit log lets a user grep history; rules are local to the user's DB (no networked propagation).
- **`alias_of` cycles.** A → B → A would loop. Mitigation: at apply time, detect cycles by tracking visited resource_ids; on detection, drop the cycling rule from this query's apply and log a warning.

---

## Test & Verification Strategy

Each implementation unit ships with its own test file (paths listed per unit). Two cross-cutting verifications gate PR readiness:

- **Speedrun regression suite.** Add a top-level `internal/cli/speedrun_test.go` that runs the five Problem Frame queries (`portugal world cup`, `bitcoin 100k`, `oscars best picture 2026`, `fed rate cut june`, `nba championship 2026`) end-to-end against a seeded fixture DB and asserts the expected top Kalshi ticker for each. This is the "did we actually fix the bug" check.
- **Live API smoke (manual, not CI).** Document in the PR description a manual checklist: run each of the five queries against the user's local DB after sync, confirm the top Kalshi hit matches expectations.

## Open Questions / Deferred

- **Auto-inference of learnings from `--pair` confirmations in `compare`.** This PR ships explicit `learn`. Auto-inference (e.g., "user ran `compare foo --pair pm-x=kalshi-y` three times in a session — auto-create an alias rule") is a follow-up after the explicit surface is exercised.
- **Shared learning across users.** Out of scope. Each user's `search_learnings` is local. A future PR could optionally publish curated rules to a shared registry.
- **MCP surface for `learn` / `forget`.** Deferred. The CLI surface ships first; MCP exposure can layer on once the shape is validated.
- **Polymarket sync coverage parity.** Polymarket's `events`/`markets` tables also cap at ~100 rows per sync. Same root pattern as Kalshi, separate PR.

---

## PR Strategy

Branch: `feat/prediction-goat-search-freshness-learning` off `feat/prediction-goat` (PR #780).
Base: `feat/prediction-goat`.
Stacked PR — do not merge until `feat/prediction-goat` lands first.

Single PR covers all eight units. Total surface ~12 modified files + ~9 new files. Each unit is its own commit for review-ability.
