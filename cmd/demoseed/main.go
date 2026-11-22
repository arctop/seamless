// demoseed is a thin CLI over internal/demokit: it seeds a THROWAWAY Seamless
// data dir with backdated fixtures. Four modes, and -scale on the review one:
//
//	go run ./cmd/demoseed -data /tmp/seamless-demo                # console fleet history (branding)
//	go run ./cmd/demoseed -review -data /tmp/seamless-review      # the fleet plus the console's review states
//	go run ./cmd/demoseed -review -scale -data /tmp/seamless-scale  # the review fixture at the owner's scale
//	go run ./cmd/demoseed -scenes -data /tmp/x -repo /tmp/myapp   # terminal-scene fixture (shared)
//	go run ./cmd/demoseed -diary -data /tmp/seamless-diary        # Diary fixture (plan:diary)
//
// Run it only against a fresh dir while no daemon holds the DB (single-writer
// SQLite); never point it at a live instance. The seeding logic lives in
// internal/demokit so the agent-scenario benchmark can reuse it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/arctop/seamless/internal/demokit"
)

// options are demoseed's flags.
type options struct {
	dataDir, repoPath                  string
	scenes, race, diary, review, scale bool
}

// parseFlags reads demoseed's flags from args (without the program name).
func parseFlags(args []string, output io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("demoseed", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&o.dataDir, "data", "", "throwaway data dir to seed (required, must not be a live instance)")
	fs.BoolVar(&o.scenes, "scenes", false, "seed the minimal terminal-scenes fixture (plan:terminal-scenes) instead of the console fleet history")
	fs.StringVar(&o.repoPath, "repo", "", "with -scenes: path of the myapp demo repo to map to the myapp project")
	fs.BoolVar(&o.race, "race", false, "with -scenes: leave two plan steps claimable so the two-agents-one-queue scene can race")
	fs.BoolVar(&o.diary, "diary", false, "seed the Diary fixture (plan:diary) instead of the console fleet history: three weeks of sessions, usage-ledger rows, plans and tasks around now, in local days")
	fs.BoolVar(&o.review, "review", false, "seed the console fleet history plus the review fixture (plan:console-ux-pass): the task, plan, project, session, mishap, held-write, lock and usage states the console must show")
	fs.BoolVar(&o.scale, "scale", false, "with -review: grow the review fixture to the owner's scale, the same on every run: about 1,500 active memories, 300 archived and superseded, 700 notes and 55 projects")
	err := fs.Parse(args)
	return o, err
}

// check refuses a combination of flags demoseed does not seed.
func (o options) check() error {
	switch {
	case o.dataDir == "":
		return errors.New("-data is required")
	case o.diary && (o.scenes || o.repoPath != "" || o.race):
		return errors.New("-diary takes none of -scenes, -repo and -race")
	case o.review && (o.diary || o.scenes || o.repoPath != "" || o.race):
		return errors.New("-review takes none of -diary, -scenes, -repo and -race")
	case o.scale && !o.review:
		return errors.New("-scale is only valid with -review")
	}
	return nil
}

func main() {
	o, err := parseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		os.Exit(2) // the flag set printed the error and the usage
	}
	if err := o.check(); err != nil {
		log.Fatalf("demoseed: %v", err)
	}

	s, err := demokit.New(o.dataDir)
	if err != nil {
		log.Fatalf("demoseed: %v", err)
	}
	defer func() { _ = s.Close() }()

	if o.scenes {
		s.Scenes(o.repoPath, o.race)
		return
	}
	if o.diary {
		fx, err := s.SeedDiary(time.Local)
		if err != nil {
			_ = s.Close()
			log.Fatalf("demoseed: %v", err)
		}
		fmt.Print(fx.Summary())
		return
	}
	if o.review && o.scale {
		fx, scale, err := s.SeedReviewScale()
		if err != nil {
			_ = s.Close()
			log.Fatalf("demoseed: %v", err)
		}
		fmt.Print(fx.Summary())
		fmt.Print(scale.Summary())
		return
	}
	if o.review {
		fx, err := s.SeedReview()
		if err != nil {
			_ = s.Close()
			log.Fatalf("demoseed: %v", err)
		}
		fmt.Print(fx.Summary())
		return
	}
	s.SeedConsoleFleet()
}
