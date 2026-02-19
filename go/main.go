package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tinygo.org/x/bluetooth"
)

var (
	controlServiceUUID         = mustParseUUID("99fa0001-338a-1024-8a49-009c0215f78a")
	controlCharacteristicUUID  = mustParseUUID("99fa0002-338a-1024-8a49-009c0215f78a")
	positionServiceUUID        = mustParseUUID("99fa0020-338a-1024-8a49-009c0215f78a")
	positionCharacteristicUUID = mustParseUUID("99fa0021-338a-1024-8a49-009c0215f78a")
)

var (
	cmdUp   = []byte{0x47, 0x00}
	cmdDown = []byte{0x46, 0x00}
	cmdStop = []byte{0xFF, 0x00}
)

type discoveredDevice struct {
	Address       bluetooth.Address
	AddressString string
	Name          string
	RSSI          int16
	LooksLikeDesk bool
}

type deskConnection struct {
	device       bluetooth.Device
	controlChar  bluetooth.DeviceCharacteristic
	positionChar bluetooth.DeviceCharacteristic
}

type deskCommandOptions struct {
	deskIdentifier    string
	scanTimeout       time.Duration
	connectTimeout    time.Duration
	verbose           bool
	useDaemon         bool
	daemonIdleTimeout time.Duration
}

type daemonRequest struct {
	Type              string  `json:"type"`
	DeskIdentifier    string  `json:"desk_identifier,omitempty"`
	TargetHeight      float64 `json:"target_height,omitempty"`
	ScanTimeoutSec    int     `json:"scan_timeout_sec,omitempty"`
	ConnectTimeoutSec int     `json:"connect_timeout_sec,omitempty"`
	Verbose           bool    `json:"verbose,omitempty"`
}

type daemonResponse struct {
	OK             bool     `json:"ok"`
	Error          string   `json:"error,omitempty"`
	Message        string   `json:"message,omitempty"`
	Height         *float64 `json:"height,omitempty"`
	Connected      bool     `json:"connected,omitempty"`
	DeskIdentifier string   `json:"desk_identifier,omitempty"`
}

type persistedState struct {
	DeskIdentifier string    `json:"desk_identifier,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

type daemonServer struct {
	adapter        *bluetooth.Adapter
	conn           *deskConnection
	activeDesk     discoveredDevice
	haveActiveDesk bool
	rememberedDesk string
	verbose        bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "scan":
		return runScan(args[1:])
	case "status":
		return runStatus(args[1:])
	case "move-to":
		return runMoveTo(args[1:])
	case "up":
		return runUp(args[1:])
	case "down":
		return runDown(args[1:])
	case "stop":
		return runStop(args[1:])
	case "daemon":
		return runDaemon(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Println(`idasen-go - IKEA IDÅSEN / LINAK BLE controller (tinygo.org/x/bluetooth)

Usage:
  idasen-go scan [--timeout 10] [--all] [--verbose]

  idasen-go status [--desk IDENTIFIER] [--scan-timeout 8] [--connect-timeout 8] [--daemon=true] [--daemon-idle-timeout 120m] [--verbose]
  idasen-go move-to [--desk IDENTIFIER] --height 45 [--min 0] [--max 58] [--scan-timeout 8] [--connect-timeout 8] [--daemon=true] [--daemon-idle-timeout 120m] [--verbose]
  idasen-go up [--desk IDENTIFIER] [--max 58] [--scan-timeout 8] [--connect-timeout 8] [--daemon=true] [--daemon-idle-timeout 120m] [--verbose]
  idasen-go down [--desk IDENTIFIER] [--min 0] [--scan-timeout 8] [--connect-timeout 8] [--daemon=true] [--daemon-idle-timeout 120m] [--verbose]
  idasen-go stop [--desk IDENTIFIER] [--scan-timeout 8] [--connect-timeout 8] [--daemon=true] [--daemon-idle-timeout 120m] [--verbose]

Daemon:
  idasen-go daemon start [--idle-timeout 120m] [--verbose]
  idasen-go daemon status
  idasen-go daemon stop

IDENTIFIER can be one of:
  - BLE MAC address (Linux/Windows)
  - CoreBluetooth UUID (macOS), e.g. cc70b52f-b68e-356b-45f0-a401bed542ce
  - advertised name, e.g. "Desk 3077"

Desk identifier default order:
  1) IDASEN_DESK env var
  2) last successful desk in ~/.cache/idasen-go/state.json (platform equivalent)

By default commands use a local daemon and keep the BLE connection warm for 120 minutes of inactivity.`)
}

func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	timeoutSec := fs.Int("timeout", 10, "scan timeout in seconds")
	all := fs.Bool("all", false, "show all BLE devices (not only desk-like)")
	verbose := fs.Bool("verbose", false, "enable verbose logs")

	if err := fs.Parse(args); err != nil {
		return err
	}

	adapter, err := enableAdapter(*verbose)
	if err != nil {
		return err
	}

	devices, err := scanForDevices(adapter, time.Duration(*timeoutSec)*time.Second, *verbose)
	if err != nil {
		return err
	}

	if !*all {
		filtered := make([]discoveredDevice, 0, len(devices))
		for _, d := range devices {
			if d.LooksLikeDesk {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}

	if len(devices) == 0 {
		fmt.Println("No desks found. Put your desk in pairing mode and retry.")
		return nil
	}

	for _, d := range devices {
		fmt.Println(formatDiscoveredDevice(d))
	}

	return nil
}

func runStatus(args []string) error {
	opts, err := parseDeskCommandOptions("status", args)
	if err != nil {
		return err
	}

	if opts.useDaemon {
		resp, err := requestDaemonAuto(daemonRequest{
			Type:              "status",
			DeskIdentifier:    opts.deskIdentifier,
			ScanTimeoutSec:    durationSeconds(opts.scanTimeout),
			ConnectTimeoutSec: durationSeconds(opts.connectTimeout),
			Verbose:           opts.verbose,
		}, opts.daemonIdleTimeout, opts.verbose)
		if err != nil {
			return err
		}
		if resp.Height == nil {
			return errors.New("daemon returned no height")
		}
		fmt.Printf("%.2f\n", *resp.Height)
		return nil
	}

	var height float64
	err = withDeskConnection(opts, func(conn *deskConnection) error {
		var readErr error
		height, _, readErr = conn.readPosition()
		return readErr
	})
	if err != nil {
		return err
	}

	fmt.Printf("%.2f\n", height)
	return nil
}

func runMoveTo(args []string) error {
	fs := flag.NewFlagSet("move-to", flag.ContinueOnError)
	desk := fs.String("desk", defaultDeskIdentifier(), "desk identifier (UUID/MAC/name)")
	height := fs.Float64("height", math.NaN(), "target height in cm")
	minHeight := fs.Float64("min", 0.0, "minimum height clamp")
	maxHeight := fs.Float64("max", 58.0, "maximum height clamp")
	scanTimeoutSec := fs.Int("scan-timeout", 8, "BLE scan timeout in seconds")
	connectTimeoutSec := fs.Int("connect-timeout", 8, "BLE connect timeout in seconds")
	useDaemon := fs.Bool("daemon", true, "use daemon for warm BLE connection")
	daemonIdleTimeout := fs.Duration("daemon-idle-timeout", 120*time.Minute, "daemon idle timeout")
	verbose := fs.Bool("verbose", false, "enable verbose logs")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if math.IsNaN(*height) {
		return errors.New("missing required --height")
	}
	if *maxHeight < *minHeight {
		return fmt.Errorf("invalid range: max %.2f < min %.2f", *maxHeight, *minHeight)
	}

	targetHeight := clamp(*height, *minHeight, *maxHeight)
	opts := deskCommandOptions{
		deskIdentifier:    strings.TrimSpace(*desk),
		scanTimeout:       time.Duration(*scanTimeoutSec) * time.Second,
		connectTimeout:    time.Duration(*connectTimeoutSec) * time.Second,
		verbose:           *verbose,
		useDaemon:         *useDaemon,
		daemonIdleTimeout: *daemonIdleTimeout,
	}

	if !opts.useDaemon && opts.deskIdentifier == "" {
		return errors.New("missing --desk (or set IDASEN_DESK)")
	}

	if opts.useDaemon {
		resp, err := requestDaemonAuto(daemonRequest{
			Type:              "move",
			DeskIdentifier:    opts.deskIdentifier,
			TargetHeight:      targetHeight,
			ScanTimeoutSec:    durationSeconds(opts.scanTimeout),
			ConnectTimeoutSec: durationSeconds(opts.connectTimeout),
			Verbose:           opts.verbose,
		}, opts.daemonIdleTimeout, opts.verbose)
		if err != nil {
			return err
		}

		if resp.Height != nil {
			fmt.Printf("Desk moved to %.2f\n", *resp.Height)
		} else {
			fmt.Println("Desk move command sent")
		}
		return nil
	}

	var final float64
	err := withDeskConnection(opts, func(conn *deskConnection) error {
		var moveErr error
		final, moveErr = moveToHeight(conn, targetHeight, opts.verbose)
		return moveErr
	})
	if err != nil {
		return err
	}

	fmt.Printf("Desk moved to %.2f\n", final)
	return nil
}

func runUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	desk := fs.String("desk", defaultDeskIdentifier(), "desk identifier (UUID/MAC/name)")
	maxHeight := fs.Float64("max", 58.0, "maximum target height")
	scanTimeoutSec := fs.Int("scan-timeout", 8, "BLE scan timeout in seconds")
	connectTimeoutSec := fs.Int("connect-timeout", 8, "BLE connect timeout in seconds")
	useDaemon := fs.Bool("daemon", true, "use daemon for warm BLE connection")
	daemonIdleTimeout := fs.Duration("daemon-idle-timeout", 120*time.Minute, "daemon idle timeout")
	verbose := fs.Bool("verbose", false, "enable verbose logs")

	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := deskCommandOptions{
		deskIdentifier:    strings.TrimSpace(*desk),
		scanTimeout:       time.Duration(*scanTimeoutSec) * time.Second,
		connectTimeout:    time.Duration(*connectTimeoutSec) * time.Second,
		verbose:           *verbose,
		useDaemon:         *useDaemon,
		daemonIdleTimeout: *daemonIdleTimeout,
	}

	if !opts.useDaemon && opts.deskIdentifier == "" {
		return errors.New("missing --desk (or set IDASEN_DESK)")
	}

	if opts.useDaemon {
		resp, err := requestDaemonAuto(daemonRequest{
			Type:              "move",
			DeskIdentifier:    opts.deskIdentifier,
			TargetHeight:      *maxHeight,
			ScanTimeoutSec:    durationSeconds(opts.scanTimeout),
			ConnectTimeoutSec: durationSeconds(opts.connectTimeout),
			Verbose:           opts.verbose,
		}, opts.daemonIdleTimeout, opts.verbose)
		if err != nil {
			return err
		}
		if resp.Height != nil {
			fmt.Printf("Desk moved up to %.2f\n", *resp.Height)
		} else {
			fmt.Println("Desk up command sent")
		}
		return nil
	}

	var final float64
	err := withDeskConnection(opts, func(conn *deskConnection) error {
		var moveErr error
		final, moveErr = moveToHeight(conn, *maxHeight, opts.verbose)
		return moveErr
	})
	if err != nil {
		return err
	}

	fmt.Printf("Desk moved up to %.2f\n", final)
	return nil
}

func runDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	desk := fs.String("desk", defaultDeskIdentifier(), "desk identifier (UUID/MAC/name)")
	minHeight := fs.Float64("min", 0.0, "minimum target height")
	scanTimeoutSec := fs.Int("scan-timeout", 8, "BLE scan timeout in seconds")
	connectTimeoutSec := fs.Int("connect-timeout", 8, "BLE connect timeout in seconds")
	useDaemon := fs.Bool("daemon", true, "use daemon for warm BLE connection")
	daemonIdleTimeout := fs.Duration("daemon-idle-timeout", 120*time.Minute, "daemon idle timeout")
	verbose := fs.Bool("verbose", false, "enable verbose logs")

	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := deskCommandOptions{
		deskIdentifier:    strings.TrimSpace(*desk),
		scanTimeout:       time.Duration(*scanTimeoutSec) * time.Second,
		connectTimeout:    time.Duration(*connectTimeoutSec) * time.Second,
		verbose:           *verbose,
		useDaemon:         *useDaemon,
		daemonIdleTimeout: *daemonIdleTimeout,
	}

	if !opts.useDaemon && opts.deskIdentifier == "" {
		return errors.New("missing --desk (or set IDASEN_DESK)")
	}

	if opts.useDaemon {
		resp, err := requestDaemonAuto(daemonRequest{
			Type:              "move",
			DeskIdentifier:    opts.deskIdentifier,
			TargetHeight:      *minHeight,
			ScanTimeoutSec:    durationSeconds(opts.scanTimeout),
			ConnectTimeoutSec: durationSeconds(opts.connectTimeout),
			Verbose:           opts.verbose,
		}, opts.daemonIdleTimeout, opts.verbose)
		if err != nil {
			return err
		}
		if resp.Height != nil {
			fmt.Printf("Desk moved down to %.2f\n", *resp.Height)
		} else {
			fmt.Println("Desk down command sent")
		}
		return nil
	}

	var final float64
	err := withDeskConnection(opts, func(conn *deskConnection) error {
		var moveErr error
		final, moveErr = moveToHeight(conn, *minHeight, opts.verbose)
		return moveErr
	})
	if err != nil {
		return err
	}

	fmt.Printf("Desk moved down to %.2f\n", final)
	return nil
}

func runStop(args []string) error {
	opts, err := parseDeskCommandOptions("stop", args)
	if err != nil {
		return err
	}

	if opts.useDaemon {
		_, err := requestDaemonAuto(daemonRequest{
			Type:              "stop",
			DeskIdentifier:    opts.deskIdentifier,
			ScanTimeoutSec:    durationSeconds(opts.scanTimeout),
			ConnectTimeoutSec: durationSeconds(opts.connectTimeout),
			Verbose:           opts.verbose,
		}, opts.daemonIdleTimeout, opts.verbose)
		if err != nil {
			return err
		}
		fmt.Println("Stop command sent")
		return nil
	}

	err = withDeskConnection(opts, func(conn *deskConnection) error {
		return conn.sendControl(cmdStop)
	})
	if err != nil {
		return err
	}

	fmt.Println("Stop command sent")
	return nil
}

func parseDeskCommandOptions(name string, args []string) (deskCommandOptions, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	desk := fs.String("desk", defaultDeskIdentifier(), "desk identifier (UUID/MAC/name)")
	scanTimeoutSec := fs.Int("scan-timeout", 8, "BLE scan timeout in seconds")
	connectTimeoutSec := fs.Int("connect-timeout", 8, "BLE connect timeout in seconds")
	useDaemon := fs.Bool("daemon", true, "use daemon for warm BLE connection")
	daemonIdleTimeout := fs.Duration("daemon-idle-timeout", 120*time.Minute, "daemon idle timeout")
	verbose := fs.Bool("verbose", false, "enable verbose logs")

	if err := fs.Parse(args); err != nil {
		return deskCommandOptions{}, err
	}

	opts := deskCommandOptions{
		deskIdentifier:    strings.TrimSpace(*desk),
		scanTimeout:       time.Duration(*scanTimeoutSec) * time.Second,
		connectTimeout:    time.Duration(*connectTimeoutSec) * time.Second,
		verbose:           *verbose,
		useDaemon:         *useDaemon,
		daemonIdleTimeout: *daemonIdleTimeout,
	}

	if !opts.useDaemon && opts.deskIdentifier == "" {
		return deskCommandOptions{}, errors.New("missing --desk (or set IDASEN_DESK)")
	}

	return opts, nil
}

func withDeskConnection(opts deskCommandOptions, fn func(*deskConnection) error) error {
	if strings.TrimSpace(opts.deskIdentifier) == "" {
		return errors.New("missing --desk (or set IDASEN_DESK)")
	}

	adapter, err := enableAdapter(opts.verbose)
	if err != nil {
		return err
	}

	target, err := resolveDesk(adapter, opts.deskIdentifier, opts.scanTimeout, opts.verbose)
	if err != nil {
		return err
	}

	conn, err := connectDesk(adapter, target, opts.connectTimeout, opts.verbose)
	if err != nil {
		return err
	}
	defer conn.close()

	if err := rememberDeskIdentifier(target.AddressString); err != nil {
		verbosef(opts.verbose, "warning: failed to persist desk identifier: %v", err)
	}

	return fn(conn)
}

func runDaemon(args []string) error {
	if len(args) == 0 {
		return runDaemonStatus(nil)
	}

	switch args[0] {
	case "start":
		return runDaemonStart(args[1:])
	case "run":
		return runDaemonRun(args[1:])
	case "status":
		return runDaemonStatus(args[1:])
	case "stop":
		return runDaemonStop(args[1:])
	default:
		return fmt.Errorf("unknown daemon command %q (expected: start|run|status|stop)", args[0])
	}
}

func runDaemonStart(args []string) error {
	fs := flag.NewFlagSet("daemon start", flag.ContinueOnError)
	idleTimeout := fs.Duration("idle-timeout", 120*time.Minute, "daemon idle timeout")
	verbose := fs.Bool("verbose", false, "enable verbose logs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	started, err := ensureDaemonRunning(*idleTimeout, *verbose)
	if err != nil {
		return err
	}
	if started {
		fmt.Println("daemon started")
	} else {
		fmt.Println("daemon already running")
	}
	return nil
}

func runDaemonRun(args []string) error {
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	idleTimeout := fs.Duration("idle-timeout", 120*time.Minute, "daemon idle timeout")
	verbose := fs.Bool("verbose", false, "enable verbose logs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	return runDaemonServer(*idleTimeout, *verbose)
}

func runDaemonStatus(args []string) error {
	fs := flag.NewFlagSet("daemon status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	resp, err := sendDaemonRequest(daemonRequest{Type: "ping"})
	if err != nil {
		if isDaemonUnavailable(err) {
			fmt.Println("daemon not running")
			return nil
		}
		return err
	}

	status := "daemon running"
	if resp.Connected {
		status += " (connected)"
	} else {
		status += " (idle)"
	}
	if strings.TrimSpace(resp.DeskIdentifier) != "" {
		status += " desk=" + resp.DeskIdentifier
	}
	fmt.Println(status)
	return nil
}

func runDaemonStop(args []string) error {
	fs := flag.NewFlagSet("daemon stop", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	resp, err := sendDaemonRequest(daemonRequest{Type: "shutdown"})
	if err != nil {
		if isDaemonUnavailable(err) {
			fmt.Println("daemon not running")
			return nil
		}
		return err
	}

	if strings.TrimSpace(resp.Message) != "" {
		fmt.Println(resp.Message)
	} else {
		fmt.Println("daemon stopped")
	}
	return nil
}

func requestDaemonAuto(req daemonRequest, idleTimeout time.Duration, verbose bool) (daemonResponse, error) {
	_, err := ensureDaemonRunning(idleTimeout, verbose)
	if err != nil {
		return daemonResponse{}, err
	}
	return sendDaemonRequest(req)
}

func ensureDaemonRunning(idleTimeout time.Duration, verbose bool) (bool, error) {
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Minute
	}

	if _, err := sendDaemonRequest(daemonRequest{Type: "ping"}); err == nil {
		return false, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("resolve executable: %w", err)
	}

	logPath, err := daemonLogPath()
	if err != nil {
		return false, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false, fmt.Errorf("open daemon log file %s: %w", logPath, err)
	}

	cmd := exec.Command(exe, "daemon", "run", "--idle-timeout", idleTimeout.String())
	if verbose {
		cmd.Args = append(cmd.Args, "--verbose")
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return false, fmt.Errorf("start daemon: %w", err)
	}
	_ = cmd.Process.Release()
	_ = logFile.Close()

	deadline := time.Now().Add(8 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		if _, err := sendDaemonRequest(daemonRequest{Type: "ping"}); err == nil {
			return true, nil
		} else {
			lastErr = err
		}
	}

	return false, fmt.Errorf("daemon did not become ready (log: %s): %w", logPath, lastErr)
}

func sendDaemonRequest(req daemonRequest) (daemonResponse, error) {
	resp, err := sendDaemonRequestRaw(req)
	if err != nil {
		return daemonResponse{}, err
	}
	if !resp.OK {
		msg := strings.TrimSpace(resp.Error)
		if msg == "" {
			msg = "daemon request failed"
		}
		return resp, errors.New(msg)
	}
	return resp, nil
}

func sendDaemonRequestRaw(req daemonRequest) (daemonResponse, error) {
	socketPath, err := daemonSocketPath()
	if err != nil {
		return daemonResponse{}, err
	}

	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return daemonResponse{}, err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(90 * time.Second))

	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return daemonResponse{}, fmt.Errorf("encode daemon request: %w", err)
	}

	var resp daemonResponse
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return daemonResponse{}, fmt.Errorf("decode daemon response: %w", err)
	}

	return resp, nil
}

func runDaemonServer(idleTimeout time.Duration, verbose bool) error {
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Minute
	}

	socketPath, err := daemonSocketPath()
	if err != nil {
		return err
	}

	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale daemon socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen daemon socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	_ = os.Chmod(socketPath, 0o600)

	server, err := newDaemonServer(verbose)
	if err != nil {
		return err
	}
	defer server.close()

	verbosef(verbose, "daemon listening on %s", socketPath)

	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		return errors.New("internal error: expected unix listener")
	}

	lastActivity := time.Now()
	for {
		_ = unixListener.SetDeadline(time.Now().Add(1 * time.Second))
		conn, err := unixListener.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if time.Since(lastActivity) >= idleTimeout {
					verbosef(verbose, "daemon idle timeout reached (%s), exiting", idleTimeout)
					return nil
				}
				continue
			}
			return fmt.Errorf("accept daemon client: %w", err)
		}

		lastActivity = time.Now()
		shutdown := server.handleClient(conn)
		_ = conn.Close()
		if shutdown {
			verbosef(verbose, "daemon shutting down on request")
			return nil
		}
	}
}

func newDaemonServer(verbose bool) (*daemonServer, error) {
	adapter, err := enableAdapter(verbose)
	if err != nil {
		return nil, err
	}

	remembered, err := loadRememberedDeskIdentifier()
	if err != nil {
		verbosef(verbose, "warning: failed to load remembered desk: %v", err)
	}

	return &daemonServer{
		adapter:        adapter,
		rememberedDesk: remembered,
		verbose:        verbose,
	}, nil
}

func (s *daemonServer) close() {
	s.dropConnection()
}

func (s *daemonServer) dropConnection() {
	if s.conn != nil {
		s.conn.close()
		s.conn = nil
	}
	s.haveActiveDesk = false
}

func (s *daemonServer) currentDeskIdentifier() string {
	if s.haveActiveDesk {
		return s.activeDesk.AddressString
	}
	return s.rememberedDesk
}

func (s *daemonServer) handleClient(conn net.Conn) bool {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req daemonRequest
	if err := dec.Decode(&req); err != nil {
		_ = enc.Encode(daemonResponse{OK: false, Error: fmt.Sprintf("decode request: %v", err)})
		return false
	}

	resp, shutdown := s.handleRequest(req)
	if err := enc.Encode(resp); err != nil {
		verbosef(s.verbose, "failed to encode daemon response: %v", err)
	}

	return shutdown
}

func (s *daemonServer) handleRequest(req daemonRequest) (daemonResponse, bool) {
	reqVerbose := req.Verbose || s.verbose

	switch req.Type {
	case "ping":
		return daemonResponse{
			OK:             true,
			Message:        "ok",
			Connected:      s.conn != nil,
			DeskIdentifier: s.currentDeskIdentifier(),
		}, false
	case "shutdown":
		s.dropConnection()
		return daemonResponse{OK: true, Message: "daemon stopped"}, true
	case "status":
		resp, err := s.withConnection(req, func(conn *deskConnection) (daemonResponse, error) {
			height, _, err := conn.readPosition()
			if err != nil {
				return daemonResponse{}, err
			}
			return daemonResponse{OK: true, Height: &height}, nil
		})
		if err != nil {
			return daemonResponse{OK: false, Error: err.Error()}, false
		}
		resp.Connected = true
		resp.DeskIdentifier = s.currentDeskIdentifier()
		return resp, false
	case "move":
		resp, err := s.withConnection(req, func(conn *deskConnection) (daemonResponse, error) {
			finalHeight, err := moveToHeight(conn, req.TargetHeight, reqVerbose)
			if err != nil {
				return daemonResponse{}, err
			}
			msg := fmt.Sprintf("Desk moved to %.2f", finalHeight)
			return daemonResponse{OK: true, Message: msg, Height: &finalHeight}, nil
		})
		if err != nil {
			return daemonResponse{OK: false, Error: err.Error()}, false
		}
		resp.Connected = true
		resp.DeskIdentifier = s.currentDeskIdentifier()
		return resp, false
	case "stop":
		_, err := s.withConnection(req, func(conn *deskConnection) (daemonResponse, error) {
			if err := conn.sendControl(cmdStop); err != nil {
				return daemonResponse{}, err
			}
			return daemonResponse{OK: true, Message: "Stop command sent"}, nil
		})
		if err != nil {
			return daemonResponse{OK: false, Error: err.Error()}, false
		}
		return daemonResponse{
			OK:             true,
			Message:        "Stop command sent",
			Connected:      true,
			DeskIdentifier: s.currentDeskIdentifier(),
		}, false
	default:
		return daemonResponse{OK: false, Error: fmt.Sprintf("unknown request type %q", req.Type)}, false
	}
}

func (s *daemonServer) withConnection(req daemonRequest, fn func(*deskConnection) (daemonResponse, error)) (daemonResponse, error) {
	conn, err := s.ensureConnection(req)
	if err != nil {
		return daemonResponse{}, err
	}

	resp, err := fn(conn)
	if err == nil {
		return resp, nil
	}

	// Retry once with a fresh connection in case we lost the BLE link.
	s.dropConnection()
	conn, reconnectErr := s.ensureConnection(req)
	if reconnectErr != nil {
		return daemonResponse{}, err
	}

	return fn(conn)
}

func (s *daemonServer) ensureConnection(req daemonRequest) (*deskConnection, error) {
	deskIdentifier := strings.TrimSpace(req.DeskIdentifier)
	if deskIdentifier == "" {
		if s.haveActiveDesk {
			deskIdentifier = s.activeDesk.AddressString
		} else {
			deskIdentifier = strings.TrimSpace(s.rememberedDesk)
		}
	}

	if deskIdentifier == "" {
		return nil, errors.New("no desk configured; pass --desk once")
	}

	if s.conn != nil && s.haveActiveDesk && matchesIdentifier(s.activeDesk, deskIdentifier) {
		return s.conn, nil
	}

	s.dropConnection()

	scanTimeout := time.Duration(req.ScanTimeoutSec) * time.Second
	if scanTimeout <= 0 {
		scanTimeout = 8 * time.Second
	}

	connectTimeout := time.Duration(req.ConnectTimeoutSec) * time.Second
	if connectTimeout <= 0 {
		connectTimeout = 8 * time.Second
	}

	verbose := req.Verbose || s.verbose
	target, err := resolveDesk(s.adapter, deskIdentifier, scanTimeout, verbose)
	if err != nil {
		return nil, err
	}

	conn, err := connectDesk(s.adapter, target, connectTimeout, verbose)
	if err != nil {
		return nil, err
	}

	s.conn = conn
	s.activeDesk = target
	s.haveActiveDesk = true
	s.rememberedDesk = target.AddressString
	if err := rememberDeskIdentifier(s.rememberedDesk); err != nil {
		verbosef(verbose, "warning: failed to persist desk identifier: %v", err)
	}

	return conn, nil
}

func enableAdapter(verbose bool) (*bluetooth.Adapter, error) {
	adapter := bluetooth.DefaultAdapter
	verbosef(verbose, "Enabling BLE adapter")
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable BLE adapter: %w", err)
	}
	return adapter, nil
}

func scanForDevices(adapter *bluetooth.Adapter, timeout time.Duration, verbose bool) ([]discoveredDevice, error) {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}

	verbosef(verbose, "Scanning for %.1fs", timeout.Seconds())

	found := make(map[string]discoveredDevice)
	stop := make(chan struct{})

	go func() {
		select {
		case <-time.After(timeout):
			_ = adapter.StopScan()
		case <-stop:
		}
	}()

	err := adapter.Scan(func(_ *bluetooth.Adapter, result bluetooth.ScanResult) {
		address := result.Address.String()
		name := strings.TrimSpace(result.LocalName())
		key := strings.ToLower(address)

		looksDesk := looksLikeDesk(name) || result.HasServiceUUID(controlServiceUUID) || result.HasServiceUUID(positionServiceUUID)

		entry := discoveredDevice{
			Address:       result.Address,
			AddressString: address,
			Name:          name,
			RSSI:          result.RSSI,
			LooksLikeDesk: looksDesk,
		}

		old, ok := found[key]
		if !ok {
			found[key] = entry
			return
		}

		if old.Name == "" && entry.Name != "" {
			old.Name = entry.Name
		}
		if entry.RSSI > old.RSSI || old.RSSI == 0 {
			old.RSSI = entry.RSSI
		}
		old.LooksLikeDesk = old.LooksLikeDesk || entry.LooksLikeDesk
		found[key] = old
	})

	close(stop)

	if err != nil {
		return nil, fmt.Errorf("scan failed: %w", err)
	}

	devices := make([]discoveredDevice, 0, len(found))
	for _, d := range found {
		devices = append(devices, d)
	}

	sort.Slice(devices, func(i, j int) bool {
		return rssiSortValue(devices[i].RSSI) > rssiSortValue(devices[j].RSSI)
	})

	return devices, nil
}

func resolveDesk(adapter *bluetooth.Adapter, identifier string, scanTimeout time.Duration, verbose bool) (discoveredDevice, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return discoveredDevice{}, errors.New("desk identifier is empty")
	}

	// Fast path for UUID/MAC identifiers: avoid a scan first.
	if looksLikeAddress(identifier) {
		if addr, err := parseAddress(identifier); err == nil {
			return discoveredDevice{
				Address:       addr,
				AddressString: addr.String(),
				Name:          "",
				RSSI:          0,
				LooksLikeDesk: true,
			}, nil
		}
	}

	devices, err := scanForDevices(adapter, scanTimeout, verbose)
	if err != nil {
		return discoveredDevice{}, err
	}

	for _, d := range devices {
		if matchesIdentifier(d, identifier) {
			verbosef(verbose, "Matched desk: %s (%s)", nonEmptyOrUnknown(d.Name), d.AddressString)
			return d, nil
		}
	}

	return discoveredDevice{}, fmt.Errorf("desk %q was not discovered during scan", identifier)
}

func connectDesk(adapter *bluetooth.Adapter, target discoveredDevice, connectTimeout time.Duration, verbose bool) (*deskConnection, error) {
	params := bluetooth.ConnectionParams{}
	if connectTimeout > 0 {
		params.ConnectionTimeout = bluetooth.NewDuration(connectTimeout)
	}

	verbosef(verbose, "Connecting to %s (%s)", nonEmptyOrUnknown(target.Name), target.AddressString)
	device, err := adapter.Connect(target.Address, params)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}

	conn, err := discoverDeskCharacteristics(device)
	if err != nil {
		_ = device.Disconnect()
		return nil, err
	}

	return conn, nil
}

func discoverDeskCharacteristics(device bluetooth.Device) (*deskConnection, error) {
	services, err := device.DiscoverServices([]bluetooth.UUID{controlServiceUUID, positionServiceUUID})
	if err != nil {
		return nil, fmt.Errorf("discover services: %w", err)
	}

	var controlService bluetooth.DeviceService
	var positionService bluetooth.DeviceService
	var haveControlService bool
	var havePositionService bool

	for _, svc := range services {
		switch svc.UUID() {
		case controlServiceUUID:
			controlService = svc
			haveControlService = true
		case positionServiceUUID:
			positionService = svc
			havePositionService = true
		}
	}

	if !haveControlService {
		return nil, errors.New("desk control service not found")
	}
	if !havePositionService {
		return nil, errors.New("desk position service not found")
	}

	controlChars, err := controlService.DiscoverCharacteristics([]bluetooth.UUID{controlCharacteristicUUID})
	if err != nil {
		return nil, fmt.Errorf("discover control characteristic: %w", err)
	}
	if len(controlChars) == 0 {
		return nil, errors.New("desk control characteristic not found")
	}

	positionChars, err := positionService.DiscoverCharacteristics([]bluetooth.UUID{positionCharacteristicUUID})
	if err != nil {
		return nil, fmt.Errorf("discover position characteristic: %w", err)
	}
	if len(positionChars) == 0 {
		return nil, errors.New("desk position characteristic not found")
	}

	return &deskConnection{
		device:       device,
		controlChar:  controlChars[0],
		positionChar: positionChars[0],
	}, nil
}

func (c *deskConnection) close() {
	_ = c.device.Disconnect()
}

func (c *deskConnection) readPosition() (height float64, speed uint16, err error) {
	buf := make([]byte, 16)
	n, err := c.positionChar.Read(buf)
	if err != nil {
		return 0, 0, fmt.Errorf("read position: %w", err)
	}
	if n < 2 {
		return 0, 0, fmt.Errorf("read position returned %d bytes", n)
	}

	raw := int16(binary.LittleEndian.Uint16(buf[:2]))
	height = float64(raw) / 100.0
	if n >= 4 {
		speed = binary.LittleEndian.Uint16(buf[2:4])
	}
	return height, speed, nil
}

func (c *deskConnection) sendControl(cmd []byte) error {
	if _, err := c.controlChar.WriteWithoutResponse(cmd); err == nil {
		return nil
	}
	if _, err := c.controlChar.Write(cmd); err != nil {
		return fmt.Errorf("write control command: %w", err)
	}
	return nil
}

func moveToHeight(conn *deskConnection, target float64, verbose bool) (float64, error) {
	current, _, err := conn.readPosition()
	if err != nil {
		return 0, err
	}

	verbosef(verbose, "Current height %.2f, target %.2f", current, target)

	if math.Abs(current-target) <= 0.8 {
		return current, nil
	}

	movingUp := target > current
	command := cmdDown
	if movingUp {
		command = cmdUp
	}

	deadline := time.Now().Add(45 * time.Second)
	lastCommandAt := time.Time{}
	lastPosition := current
	stagnantReads := 0

	for time.Now().Before(deadline) {
		if movingUp {
			if current >= target-0.8 {
				break
			}
		} else if current <= target+0.8 {
			break
		}

		if time.Since(lastCommandAt) >= 300*time.Millisecond {
			if err := conn.sendControl(command); err != nil {
				return current, err
			}
			lastCommandAt = time.Now()
		}

		time.Sleep(120 * time.Millisecond)

		next, speed, err := conn.readPosition()
		if err != nil {
			verbosef(verbose, "position read failed during move: %v", err)
			continue
		}

		current = next
		verbosef(verbose, "height %.2f speed %d", current, speed)

		if math.Abs(current-lastPosition) < 0.02 {
			stagnantReads++
		} else {
			stagnantReads = 0
		}
		lastPosition = current

		if stagnantReads >= 12 {
			verbosef(verbose, "movement appears stalled; stopping")
			break
		}
	}

	_ = conn.sendControl(cmdStop)
	time.Sleep(150 * time.Millisecond)

	final, _, err := conn.readPosition()
	if err == nil {
		current = final
	}

	return current, nil
}

func formatDiscoveredDevice(d discoveredDevice) string {
	name := nonEmptyOrUnknown(d.Name)
	addressDisplay := d.AddressString
	idDisplay := d.AddressString

	if looksLikeUUID(addressDisplay) {
		addressDisplay = "00:00:00:00:00:00"
		idDisplay = strings.ToLower(idDisplay)
	} else {
		addressDisplay = strings.ToUpper(addressDisplay)
		idDisplay = addressDisplay
	}

	rssiDisplay := "?"
	if d.RSSI != 0 {
		rssiDisplay = fmt.Sprintf("%d", d.RSSI)
	}

	return fmt.Sprintf("%s [address: %s, id: %s, RSSI: %s]", name, addressDisplay, idDisplay, rssiDisplay)
}

func matchesIdentifier(d discoveredDevice, identifier string) bool {
	needle := normalize(identifier)
	if needle == "" {
		return false
	}

	candidates := []string{
		normalize(d.AddressString),
		normalize(strings.ReplaceAll(d.AddressString, "-", "")),
		normalize(d.Name),
	}

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if needle == candidate {
			return true
		}
	}

	if d.Name != "" && strings.Contains(strings.ToLower(d.Name), needle) {
		return true
	}

	return false
}

func looksLikeDesk(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(name, "desk") || strings.Contains(name, "idasen") || strings.Contains(name, "linak")
}

func parseAddress(identifier string) (bluetooth.Address, error) {
	var addr bluetooth.Address
	addr.Set(strings.TrimSpace(identifier))
	if isZeroAddress(addr.String()) {
		return bluetooth.Address{}, fmt.Errorf("identifier %q is not a valid address for this platform", identifier)
	}
	return addr, nil
}

func looksLikeAddress(identifier string) bool {
	identifier = strings.TrimSpace(identifier)
	return looksLikeUUID(identifier) || looksLikeMAC(identifier)
}

func looksLikeUUID(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Count(s, "-") != 4 {
		return false
	}
	_, err := bluetooth.ParseUUID(s)
	return err == nil
}

func looksLikeMAC(s string) bool {
	s = strings.TrimSpace(strings.ToUpper(s))
	if strings.Count(s, ":") != 5 {
		return false
	}
	_, err := bluetooth.ParseMAC(s)
	return err == nil
}

func isZeroAddress(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "" || s == "00:00:00:00:00:00" || s == "00000000-0000-0000-0000-000000000000"
}

func mustParseUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return u
}

func clamp(v, minV, maxV float64) float64 {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

func rssiSortValue(rssi int16) int {
	if rssi == 0 {
		return -999
	}
	return int(rssi)
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func nonEmptyOrUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "Unknown"
	}
	return s
}

func verbosef(enabled bool, format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func durationSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d / time.Second)
}

func defaultDeskIdentifier() string {
	if env := strings.TrimSpace(os.Getenv("IDASEN_DESK")); env != "" {
		return env
	}
	remembered, err := loadRememberedDeskIdentifier()
	if err != nil {
		return ""
	}
	return remembered
}

func loadRememberedDeskIdentifier() (string, error) {
	state, err := loadPersistedState()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(state.DeskIdentifier), nil
}

func rememberDeskIdentifier(identifier string) error {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil
	}

	state, _ := loadPersistedState()
	state.DeskIdentifier = identifier
	state.UpdatedAt = time.Now()
	return savePersistedState(state)
}

func loadPersistedState() (persistedState, error) {
	path, err := daemonStatePath()
	if err != nil {
		return persistedState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return persistedState{}, nil
		}
		return persistedState{}, err
	}

	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		// Don't hard fail if file is corrupted; just start fresh.
		return persistedState{}, nil
	}

	return state, nil
}

func savePersistedState(state persistedState) error {
	path, err := daemonStatePath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return os.WriteFile(path, data, 0o600)
}

func daemonRuntimeDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "idasen-go")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func daemonSocketPath() (string, error) {
	dir, err := daemonRuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

func daemonStatePath() (string, error) {
	dir, err := daemonRuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}

func daemonLogPath() (string, error) {
	dir, err := daemonRuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}

func isDaemonUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connect: not a socket")
}
