# gen2 (Go + tinygo.org/x/bluetooth)

IKEA IDÅSEN / LINAK desk controller implemented in Go using [`tinygo.org/x/bluetooth`](https://github.com/tinygo-org/bluetooth).

## Features

- BLE scan for desk-like devices
- Connect by identifier (`UUID`, `MAC`, or advertised name)
- Read current desk position
- Move to target height (`move-to`, `up`, `down`, `stop`)
- **Daemon mode**: keeps a warm BLE connection alive for 120 minutes of inactivity (default)
- Remembers last successful desk identifier

## Build

```bash
go build .
```

## Usage

```bash
# scan nearby desks
go run . scan --timeout 10

# read current height (uses daemon by default)
go run . status --desk cc70b52f-b68e-356b-45f0-a401bed542ce

# subsequent commands can omit --desk (remembered desk)
go run . status

# move to explicit height
go run . move-to --height 45

# move to configured limits
go run . up --max 58
go run . down --min 0

# emergency stop
go run . stop
```

## Daemon control

```bash
# start daemon explicitly (optional; commands auto-start it)
go run . daemon start --idle-timeout 120m

# check daemon status
go run . daemon status

# stop daemon
go run . daemon stop
```

### Disable daemon per command

```bash
go run . status --daemon=false --desk cc70b52f-b68e-356b-45f0-a401bed542ce
```

## Identifier priority

When `--desk` is omitted, the tool uses:

1. `IDASEN_DESK` env var
2. last successful desk identifier stored in cache state
