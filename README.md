# Filing radar

Every new SEC filing, read by AI agents within minutes, explained with sources,
and checked against how the stock actually moved the next day.

**Status: stage 3, analyst notes.** Every minute the worker reads EDGAR's
latest filings and stores each one once. Plain rules label every filing high,
medium, low or routine from its form and 8-K item numbers; a language model
(DeepSeek by default, Claude as an option) then reads each 8-K and scores it,
with a quote that must appear in the filing. For every high 8-K the same model
writes a short note on what changed since the company's previous comparable
8-K; every claim quotes its source word for word, and code checks each quote
before the note is shown. All model calls are recorded with their cost and
held to a daily budget. The feed opens on the important ones. All three
servers are deployed by one pipeline, and a status page shows the live health
of every part.

This repository is public: https://github.com/Yerassyl20036/filing-radar.

## Architecture

```mermaid
flowchart TD
    subgraph FRA["Frankfurt"]
        CADDY["Caddy<br/>HTTPS"] --> WEB["web<br/>Go"]
        WEB --> PG[("Postgres 16")]
    end
    EDGAR["SEC EDGAR<br/>latest filings"]
    subgraph AS1["Ashburn-1"]
        WORKER["worker<br/>EDGAR poller + Temporal worker"] --> T1["pg-tunnel"]
        WORKER --> T2["temporal-tunnel"]
    end
    subgraph AS2["Ashburn-2"]
        TEMPORAL["Temporal server<br/>single binary, SQLite"]
    end
    USER["Visitors"] --> CADDY
    EDGAR -->|"every 60 s, at most 1 request/s"| WORKER
    T1 -->|"SSH as tunnel user"| PG
    T2 -->|"SSH as tunnel user"| TEMPORAL
```

| Server | Runs | Open to the internet |
|---|---|---|
| Frankfurt | Postgres, web, Caddy | 80, 443 (Caddy) |
| Ashburn-1 | worker (EDGAR poller, Temporal worker), two SSH tunnels | nothing |
| Ashburn-2 | Temporal server | nothing |

**Why this shape:**
- **Every server has 1 GB of RAM.** Each Go binary uses tens of MB, Temporal's single-binary server about 60 MB. Kubernetes or Argo CD would not fit.
- **No database port is public.** The worker reaches Postgres and Temporal through SSH tunnels. The `tunnel` user on each server has no shell and may forward to exactly one local port.
- **The worker proves the whole chain every minute.** It runs a tiny Temporal workflow end to end and writes the result to Postgres; the status page reads it.
- **Each filing is stored exactly once.** The accession number is the primary key and every insert is `ON CONFLICT DO NOTHING`, so retries and overlapping polls are harmless.
- **No gaps.** A poll pages back until it reaches filings already stored (at most 5 pages), and stores only after paging stops: a Form 4's entries can straddle two pages, and storing page by page would stop too early.
- **One Temporal workflow per tracked filing.** The workflow ID is the accession number, so Temporal refuses a second one while it runs. A dispatcher runs after every poll and starts a workflow for any filing without a label, so a crash or a Temporal outage heals by itself. Form 4s and other routine forms are labelled on the spot and never start one.
- **Rules first, model second.** Rules are free and explainable, and they label everything. The model reads only 8-Ks, under a daily cap, and can raise a label but never lower it. Every score carries a quote that's checked against the filing; a quote that isn't there means the score is dropped.
- **Notes are checked, not trusted.** The analyst compares the filing (main document and press release) with the company's previous 8-K sharing its lead item. Code checks every claim's quote against the document it names; failed claims get one repair, then are dropped, and a note with fewer than 2 checked claims is kept as rejected and never shown. At most two model steps per note, and every outcome but a spent budget leaves a note row, so a finished note is never paid for again.
- **A hard daily budget.** Every model call is recorded with its tokens and its cost at list price, and no call starts once the day's spend (UTC) reaches `MODEL_DAILY_BUDGET_USD`. The DeepSeek balance is prepaid with no top-up, so the budget is the only thing between a busy news day and an empty balance. The check comes before each call, so a day can overrun by the few calls already in flight (about a cent each). At the default US$0.20 a day, US$5 lasts about 25 days.
- **The model is swappable.** The workflow talks to one small interface (score, reason, quote); DeepSeek and Claude are two implementations sharing the prompt and the checks. Filings are public data, so the choice is about cost and quality.

## Deploying

The `deploy` workflow runs by hand (Actions → deploy → Run workflow):
1. Test, then build two images and push them to GHCR.
2. Deploy Temporal, then Frankfurt, then the worker, each over SSH as the `deploy` user.
3. Wait until the worker has reported through both tunnels, then until the ingestor has read EDGAR.

Its settings live in the `production` environment (Settings → Environments),
which only accepts deploys from `main`:

| Kind | Name | What it is |
|---|---|---|
| Secret | `DEPLOY_SSH_KEY_B64` | SSH key for the `deploy` user on all three servers |
| Secret | `TUNNEL_SSH_KEY_B64` | SSH key for the `tunnel` user, used by the worker's tunnels |
| Secret | `KNOWN_HOSTS_B64` | the three servers' host keys, so SSH can't be pointed at an impostor |
| Secret | `POSTGRES_PASSWORD` | database password |
| Secret | `EDGAR_USER_AGENT` | `Filing Radar <your email>`: SEC refuses requests without a contact |
| Secret | `DEEPSEEK_API_KEY` | optional: the model that scores 8-Ks (default model `deepseek-chat`) |
| Secret | `GEMINI_API_KEY`, `GROQ_API_KEY` | optional: free tiers instead (`gemini-3.5-flash-lite`, `openai/gpt-oss-120b`); Groq's free tier fits about 30-40 filings a day |
| Secret | `ANTHROPIC_API_KEY` | optional: Claude instead (default model `claude-opus-5-5`); with no key at all the rules label everything on their own |
| Variable | `TRIAGE_PROVIDER` | optional: `deepseek`, `gemini`, `groq` or `anthropic`; empty means the first with a key, in that order |
| Variable | `TRIAGE_MODEL` | optional: overrides the provider's default model |
| Variable | `TRIAGE_MODEL_DAILY_CAP` | optional, default 200 model scores per UTC day |
| Variable | `MODEL_DAILY_BUDGET_USD` | optional, default 0.20: the most all model calls (scores and notes) may cost per UTC day, in US$ |
| Variable | `ANALYST_DAILY_CAP` | optional, default 30 notes per UTC day |
| Variable | `MODEL_PRICE_IN_PER_M`, `MODEL_PRICE_OUT_PER_M` | optional, set both or neither: the model's price in US$ per million input and output tokens, when the built-in list price is out of date |
| Variable | `SITE_DOMAIN` | the site's name; Caddy gets its certificate |
| Secret | `FRANKFURT_HOST`, `WORKER_HOST`, `TEMPORAL_HOST` | server IPs; secrets, so the public Actions logs show them as *** |

To move to a real domain, point its A record at the Frankfurt IP, change
`SITE_DOMAIN`, and deploy again.

## Running locally

```bash
export EDGAR_USER_AGENT="Filing Radar you@example.com"
docker compose -f deploy/local/compose.yml up --build
```

Then open http://localhost:8088. The Temporal UI is at http://localhost:8233.

Tests: `go test ./...`. The store tests also run against Postgres when
`TEST_DATABASE_URL` is set (see `internal/store/filings_test.go`).

## API

| Endpoint | Returns |
|---|---|
| `GET /api/filings?form=8-K&level=medium&limit=50&before=<cursor>` | filings newest first, each with its `triage` label (null while pending) and its published analyst `note` (null when there is none: headline, claims each with `text`, `quote`, `source` and `url`); `level` means that level and above; `next` is the cursor for the following page |
| `GET /api/status` | the health of every part, as the status page shows it |
| `GET /api/costs` | model spend from the `model_calls` table: `all_time` and `today` (since midnight UTC), each with `usd`, `calls`, `input_tokens`, `output_tokens` and `since` (the first call, null when there are none), plus `by_purpose` (`score`, `draft`, `repair`) |
| `GET /healthz` | `ok` when the web server can reach Postgres |

## What it costs

**Running.** The three servers are Oracle Cloud free-tier VMs ($0), the
domain is sslip.io ($0), and SEC EDGAR data is free. The only running cost is
the language model: every call is recorded with its tokens and cost, the live
totals are on the site's How it works page (`/how-it-works#costs`) and at
`GET /api/costs`, and a daily budget
(`MODEL_DAILY_BUDGET_USD`, $0.20 by default) caps it.

**Development.** Built in about three days with Claude Code (Claude Opus 5.5,
a little Claude Haiku 5.5) as the main developer under the owner's direction.
Measured from the session transcripts: about 1.0 million output tokens and
219 million cached-context tokens across 710 model responses, roughly $82 at
Anthropic's API list prices. It was paid through a Claude subscription, not
per token, so that figure is the list-price equivalent, not a bill.

**Built with:** [Temporal](https://docs.temporal.io/),
[DeepSeek API](https://api-docs.deepseek.com/),
[SEC EDGAR APIs](https://www.sec.gov/search-filings/edgar-application-programming-interfaces),
[Go](https://go.dev/), [PostgreSQL](https://www.postgresql.org/),
[Caddy](https://caddyserver.com/),
[Oracle Cloud Free Tier](https://www.oracle.com/cloud/free/).

## Roadmap

1. **Live filing feed, no AI.** ✅ The worker polls EDGAR's latest-filings feed every minute, stores each filing once (keyed by its accession number), and the website shows the feed.
2. **Triage.** ✅ Plain rules read each 8-K's item numbers and label every filing; a language model scores each 8-K with a checked quote and can raise the label.
3. **Analyst agent.** ✅ For material filings: what changed since the last filing, with a citation for every claim, and every citation checked against the filing's text.
4. **Evals.** A Temporal workflow per filing sleeps until the next market close, fetches the price, and scores the prediction. Scores are public.
5. **Live architecture page** with throughput, queue depth, agent time and cost.

## Layout

```
cmd/web          website and API (Frankfurt)
cmd/worker       EDGAR poller, Temporal worker and health reports (Ashburn-1)
internal/edgar   EDGAR client and feed parser
internal/ingest  one poll: page until overlap, then store
internal/triage  plain rules that label each filing
internal/pipeline the per-filing Temporal workflows (triage, note) and their dispatcher
internal/model   the model calls that score an 8-K and draft a note (any OpenAI-compatible API, or Claude), and the quote checks
internal/store   Postgres access and schema
internal/status  turns health reports into what the status page shows
internal/ping    the one-minute Temporal round trip
deploy/          one folder per server, plus the tunnel image and local setup
```
