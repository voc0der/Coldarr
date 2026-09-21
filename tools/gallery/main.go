// Command gallery stands up a fake environment for Coldarr to run against,
// so its web GUI can be screenshotted (or poked at) with realistic data
// and no real media server anywhere near it:
//
//   - fake drives: FUSE loopback mounts whose statfs reports the fixture's
//     capacity and usage (see drive.go), so a "16TB" satellite at 89% costs
//     a few KB of sparse files
//   - fake Radarr, Sonarr and Jellyfin APIs serving the fixture's library,
//     whose moves really copy between those drives over a few seconds
//
// It runs inside the gallery container (see Dockerfile and entrypoint.sh),
// which is where the FUSE mounts live; Coldarr runs in the same container
// so it sees them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// logf logs as the gallery. Request URIs reach it (see logRequests), %q-quoted.
func logf(format string, args ...any) { log.Printf("gallery: "+format, args...) } //nolint:gosec // local fake services only

func main() {
	fixturePath := flag.String("fixture", "fixture.yaml", "fixture describing the drives and library")
	coldarrConfig := flag.String("coldarr-config", "coldarr.yaml", "the coldarr.yaml Coldarr will run with - its tier paths are created on the drives")
	backingRoot := flag.String("backing", "/var/lib/gallery", "directory holding each fake drive's real (sparse) contents")
	historyOut := flag.String("history-out", "", "write a seeded move history here, in Coldarr's history.json format")
	readyFile := flag.String("ready-file", "", "create this file once the drives are mounted and the APIs are listening")
	radarrAddr := flag.String("radarr", ":7878", "fake Radarr listen address")
	sonarrAddr := flag.String("sonarr", ":8989", "fake Sonarr listen address")
	jellyfinAddr := flag.String("jellyfin", ":8096", "fake Jellyfin listen address")
	flag.Parse()

	if err := run(*fixturePath, *coldarrConfig, *backingRoot, *historyOut, *readyFile, *radarrAddr, *sonarrAddr, *jellyfinAddr); err != nil {
		log.Fatalf("gallery: %v", err)
	}
}

func run(fixturePath, coldarrConfig, backingRoot, historyOut, readyFile, radarrAddr, sonarrAddr, jellyfinAddr string) error {
	fx, err := loadFixture(fixturePath)
	if err != nil {
		return err
	}
	tiers, err := loadTiers(coldarrConfig)
	if err != nil {
		return err
	}

	var drives []*drive
	for _, spec := range fx.Drives {
		drives = append(drives, newDrive(spec, backingRoot))
	}
	defer func() {
		for _, d := range drives {
			d.unmount()
		}
	}()

	lib, err := newLibrary(fx, drives, time.Now())
	if err != nil {
		return err
	}

	// Tier paths are created twice: under the mountpoint before mounting,
	// and on the drive after. Unmounting a drive then leaves what a dead
	// drive leaves in Docker - its tier directories still there, empty, on
	// the system disk - which is exactly what Coldarr's mount check is for.
	var tierPaths []string
	for _, t := range tiers {
		for _, p := range t.Paths {
			if lib.driveOf(p) == nil {
				return fmt.Errorf("tier %s path %s is not on any fixture drive", t.Name, p)
			}
			tierPaths = append(tierPaths, filepath.Clean(p))
		}
	}
	for _, d := range drives {
		for _, p := range tierPaths {
			if d.contains(p) {
				if err := os.MkdirAll(p, 0o750); err != nil {
					return err
				}
			}
		}
		if err := d.mountFS(); err != nil {
			return err
		}
	}
	for _, p := range tierPaths {
		if err := os.MkdirAll(p, 0o750); err != nil {
			return err
		}
	}
	lib.rootFolders = tierPaths

	if err := lib.seed(fx.Orphans); err != nil {
		return fmt.Errorf("seeding library: %w", err)
	}
	for _, d := range drives {
		if err := d.calibrate(); err != nil {
			return err
		}
		used, total := d.usage()
		logf("drive %-6s %-12s %s of %s used (%.1f%%)", d.name, d.mount, gib(used), gib(total), float64(used)/float64(total)*100)
	}
	logf("library: %d movies, %d series, %d orphan folders", len(lib.movies), len(lib.series), len(fx.Orphans))

	if historyOut != "" {
		if err := lib.writeHistory(historyOut, tiers); err != nil {
			return fmt.Errorf("writing history: %w", err)
		}
	}

	servers := []*http.Server{
		{Addr: radarrAddr, Handler: lib.arrHandler(radarr, fx.Versions.Radarr), ReadHeaderTimeout: 10 * time.Second},
		{Addr: sonarrAddr, Handler: lib.arrHandler(sonarr, fx.Versions.Sonarr), ReadHeaderTimeout: 10 * time.Second},
		{Addr: jellyfinAddr, Handler: lib.jellyfinHandler(fx.Versions.Jellyfin), ReadHeaderTimeout: 10 * time.Second},
	}
	errs := make(chan error, len(servers))
	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		go func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	logf("fake Radarr on %s, Sonarr on %s, Jellyfin on %s", radarrAddr, sonarrAddr, jellyfinAddr)

	if readyFile != "" {
		if err := os.WriteFile(readyFile, nil, 0o644); err != nil { //nolint:gosec // a marker file, nothing secret
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-errs:
		return err
	}
	for _, srv := range servers {
		_ = srv.Close()
	}
	return nil
}
