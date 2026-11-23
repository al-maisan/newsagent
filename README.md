# newsagent

Watches tech news sites and your X home timeline, and keeps you up to date on Telegram.

- **Sources:** Hacker News top stories, any RSS/Atom feed, and your X home timeline (official API).
- **Ranking:** Claude rates every new item 0–10 against your interests (set in the config).
- **Urgent pings:** items scoring 9 or higher are sent right away, at most 3 per poll, with repeats of the same story skipped.
- **Digests:** twice a day, the items scoring 6 or higher are grouped into short themed sections.

## How it works

| Command | Run by | What it does |
|---|---|---|
| `newsagent poll` | `newsagent-poll.timer` (every 15 min) | fetch sources → store new items in SQLite → score with Claude → send urgent pings |
| `newsagent digest` | `newsagent-digest.timer` (08:00, 18:00 Europe/Istanbul) | group unsent notable items into a digest → send |

- Links in messages always come from the fetched data, never from model output. Feed text is treated as untrusted data.
- An item is marked as sent only after Telegram accepts it, so failed sends are retried on the next run.
- X bills per post read, so reads are capped per poll and per UTC day (`x.max_posts_per_poll`, `x.max_posts_per_day`). If more posts arrive than the cap allows, the excess is skipped.

## Build and test

```sh
make test && make build
```

## Setup

See [docs/setup.md](docs/setup.md).
