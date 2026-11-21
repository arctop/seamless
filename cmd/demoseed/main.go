// demoseed is a thin CLI over internal/demokit: it seeds a THROWAWAY Seamless
// data dir with backdated fixtures. Three modes:
//
//	go run ./cmd/demoseed -data /tmp/seamless-demo                # console fleet history (branding)
//	go run ./cmd/demoseed -scenes -data /tmp/x -repo /tmp/myapp   # terminal-scene fixture (shared)
//	go run ./cmd/demoseed -diary -data /tmp/seamless-diary        # Diary fixture (plan:diary)
//
// Run it only against a fresh dir while no daemon holds the DB (single-writer
// SQLite); never point it at a live instance. The seeding logic lives in
// internal/demokit so the agent-scenario benchmark can reuse it.
package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/arctop/seamless/internal/demokit"
)

func main() {
	dataDir := flag.String("data", "", "throwaway data dir to seed (required, must not be a live instance)")
	scenesMode := flag.Bool("scenes", false, "seed the minimal terminal-scenes fixture (plan:terminal-scenes) instead of the console fleet history")
	repoPath := flag.String("repo", "", "with -scenes: path of the myapp demo repo to map to the myapp project")
	race := flag.Bool("race", false, "with -scenes: leave two plan steps claimable so the two-agents-one-queue scene can race")
	diaryMode := flag.Bool("diary", false, "seed the Diary fixture (plan:diary) instead of the console fleet history: three weeks of sessions, usage-ledger rows, plans and tasks around now, in local days")
	flag.Parse()
	if *dataDir == "" {
		log.Fatal("demoseed: -data is required")
	}
	if *diaryMode && (*scenesMode || *repoPath != "" || *race) {
		log.Fatal("demoseed: -diary takes none of -scenes, -repo and -race")
	}

	s, err := demokit.New(*dataDir)
	if err != nil {
		log.Fatalf("demoseed: %v", err)
	}
	defer func() { _ = s.Close() }()

	if *scenesMode {
		s.Scenes(*repoPath, *race)
		return
	}
	if *diaryMode {
		fx, err := s.SeedDiary(time.Local)
		if err != nil {
			_ = s.Close()
			log.Fatalf("demoseed: %v", err)
		}
		fmt.Print(fx.Summary())
		return
	}
	s.SeedConsoleFleet()
}
