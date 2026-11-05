package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/live"
	"github.com/awsfanboy/replan/internal/plan"
	"github.com/awsfanboy/replan/internal/sim"
	"github.com/awsfanboy/replan/internal/spec"
	"github.com/awsfanboy/replan/internal/ui"
)

// liveInput loads everything the live commands need.
func liveInput(ctx context.Context, o *opts) (live.Input, *catalog.Catalog, error) {
	if err := o.resolveEvent(); err != nil {
		return live.Input{}, nil, err
	}
	cat, err := loadCatalog(ctx, o)
	if err != nil {
		return live.Input{}, nil, err
	}
	svc, now, err := backend(cat, o)
	if err != nil {
		return live.Input{}, nil, err
	}
	sched, err := svc.GetSchedule(ctx, cat.Event.EventID)
	if err != nil {
		return live.Input{}, nil, err
	}
	in := live.Input{Catalog: cat, Schedule: sched, Now: now, At: o.at, Late: o.late, IgnoreSeatBands: o.sim && o.simOpen}
	if sp, err := spec.Load(o.file); err == nil {
		in.Spec = sp
		in.Matrix = plan.MatrixFor(sp.Rules)
	}
	return in, cat, nil
}

func liveUI(cat *catalog.Catalog) *ui.UI {
	u := ui.New(os.Stdout)
	u.Loc = cat.Location
	return u
}

func cmdNow(ctx context.Context, args []string) error {
	var o opts
	parse(flags("now", &o), args)
	in, cat, err := liveInput(ctx, &o)
	if err != nil {
		return err
	}
	liveUI(cat).LiveNext(live.Next(in), newApp(&o).Mode())
	return nil
}

func cmdTonight(ctx context.Context, args []string) error {
	var o opts
	parse(flags("tonight", &o), args)
	in, cat, err := liveInput(ctx, &o)
	if err != nil {
		return err
	}
	day := o.day
	if day == "" {
		day = live.NextEventDay(in)
	}
	liveUI(cat).Tonight(day, live.Tonight(in, day))
	return nil
}

func cmdChanges(ctx context.Context, args []string) error {
	var o opts
	parse(flags("changes", &o), args)
	if o.compare == "" && o.catalog == "" {
		o.refresh = true // fetch now so the history includes the latest changes
	}
	in, cat, err := liveInput(ctx, &o)
	if err != nil {
		return err
	}
	var changes []catalog.Change
	if o.compare != "" {
		old, err := catalog.LoadFile(o.compare)
		if err != nil {
			return err
		}
		changes = catalog.Diff(old, cat, time.Now())
	} else {
		d, err := time.ParseDuration(o.since)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		if changes, err = catalog.History("", cat.Event.EventID, time.Now().Add(-d)); err != nil {
			return err
		}
	}
	imps, unrelated := live.Impacts(in, changes)
	liveUI(cat).Changes(imps, unrelated, len(changes))
	return nil
}

func cmdSkip(cmd string, args []string) error {
	var o opts
	codes := parse(flags(cmd, &o), args)
	if len(codes) == 0 {
		return fmt.Errorf("usage: replan %s CODE [CODE...]", cmd)
	}
	sp, err := spec.Load(o.file)
	if err != nil {
		return err
	}
	cat, err := loadCatalog(context.Background(), &opts{event: sp.Event, catalog: o.catalog})
	if err != nil {
		return err
	}
	for _, c := range codes {
		s, ok := cat.Lookup(c)
		if !ok {
			return fmt.Errorf("%s is not in the %s catalog", c, sp.Event)
		}
		var out []string
		for _, x := range sp.Skip {
			if !strings.EqualFold(x, s.Code) {
				out = append(out, x)
			}
		}
		if cmd == "skip" {
			out = append(out, s.Code)
		}
		sp.Skip = out
	}
	if err := sp.Save(o.file); err != nil {
		return err
	}
	if cmd == "skip" {
		fmt.Printf("Skipping %s. Run `replan plan` to see the seat go back, then `replan apply`.\n", strings.Join(codes, ", "))
	} else {
		fmt.Printf("No longer skipping %s.\n", strings.Join(codes, ", "))
	}
	return nil
}

func cmdSimTool(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "shake" {
		return errors.New("usage: replan sim shake --sim [-f reinvent.yaml] [-o shaken.json]")
	}
	var o opts
	parse(flags("sim shake", &o), args[1:])
	if !o.sim {
		return errors.New("sim shake works on the simulator's schedule: add --sim")
	}
	if err := o.resolveEvent(); err != nil {
		return err
	}
	cat, err := loadCatalog(ctx, &o)
	if err != nil {
		return err
	}
	svc, _, err := backend(cat, &o)
	if err != nil {
		return err
	}
	sched, err := svc.GetSchedule(ctx, cat.Event.EventID)
	if err != nil {
		return err
	}
	shaken, notes, err := sim.Shake(cat, sched.Reserved)
	if err != nil {
		return err
	}
	out := o.out
	if out == "" {
		out = "catalog-shaken.json"
	}
	base := strings.TrimSuffix(out, ".json") + "-base.json"
	if err := cat.SaveFile(base); err != nil {
		return err
	}
	if err := shaken.SaveFile(out); err != nil {
		return err
	}
	fmt.Println("Simulated event-week changes:")
	for _, n := range notes {
		fmt.Println("  • " + n)
	}
	fmt.Printf("\nSee what they mean for you:\n  replan changes --sim -f %s --catalog %s --compare %s\n", o.file, out, base)
	return nil
}
