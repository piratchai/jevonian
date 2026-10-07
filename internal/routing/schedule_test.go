package routing

import (
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

func night() *config.ScheduleConfig {
	return &config.ScheduleConfig{
		Timezone: "Asia/Singapore", // UTC+8, no daylight saving
		Windows:  []config.ScheduleWindow{{ID: "night", Label: "Off-nights", Start: "22:00", End: "08:00"}},
	}
}

func utc(value string) time.Time {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return at
}

func activeID(s *config.ScheduleConfig, at time.Time) string {
	if w := ActiveWindow(s, at); w != nil {
		return w.ID
	}
	return ""
}

func TestActiveWindowRunsPastMidnightInTheScheduleZone(t *testing.T) {
	s := night()
	for at, want := range map[string]string{
		"2026-10-07T13:59:00Z": "",      // 21:59 in Singapore
		"2026-10-07T14:00:00Z": "night", // 22:00 is included
		"2026-10-07T20:00:00Z": "night", // 04:00 the next day
		"2026-10-07T23:59:00Z": "night", // 07:59
		"2026-10-08T00:00:00Z": "",      // 08:00 is excluded
		"2026-10-08T05:00:00Z": "",      // 13:00
	} {
		if got := activeID(s, utc(at)); got != want {
			t.Errorf("at %s: active = %q, want %q", at, got, want)
		}
	}
}

func TestActiveWindowSameDayRangeAndFirstMatchWins(t *testing.T) {
	s := &config.ScheduleConfig{Timezone: "UTC", Windows: []config.ScheduleWindow{
		{ID: "work", Start: "09:00", End: "17:00"},
		{ID: "lunch", Start: "12:00", End: "13:00"}, // overlaps work; work is listed first
	}}
	for at, want := range map[string]string{
		"2026-10-07T08:59:00Z": "",
		"2026-10-07T09:00:00Z": "work",
		"2026-10-07T12:30:00Z": "work",
		"2026-10-07T17:00:00Z": "",
	} {
		if got := activeID(s, utc(at)); got != want {
			t.Errorf("at %s: active = %q, want %q", at, got, want)
		}
	}
	if ActiveWindow(nil, utc("2026-10-07T12:00:00Z")) != nil {
		t.Error("no schedule must mean no active window")
	}
}

func TestStatusReportsNextChange(t *testing.T) {
	s := night()
	before := Status(s, utc("2026-10-07T13:00:00Z")) // 21:00 in Singapore
	if before.Active != "" || before.NextActive != "night" || !strings.HasPrefix(before.NextChange, "2026-10-07T22:00:00+08:00") {
		t.Fatalf("before the window: %+v", before)
	}
	during := Status(s, utc("2026-10-07T15:00:00Z")) // 23:00
	if during.Active != "night" || during.ActiveLabel != "Off-nights" || during.NextActive != "" || !strings.HasPrefix(during.NextChange, "2026-10-08T08:00:00+08:00") {
		t.Fatalf("inside the window: %+v", during)
	}
	if during.Timezone != "Asia/Singapore" || !strings.HasPrefix(during.Now, "2026-10-07T23:00:00+08:00") {
		t.Fatalf("zone and clock: %+v", during)
	}
	if Status(nil, utc("2026-10-07T15:00:00Z")) != nil {
		t.Error("no schedule must give no status")
	}
}

func TestStatusAcrossADaylightSavingChange(t *testing.T) {
	s := &config.ScheduleConfig{Timezone: "America/New_York", Windows: []config.ScheduleWindow{{ID: "night", Start: "22:00", End: "08:00"}}}
	// 2026-11-01 05:30 UTC is 01:30 EDT, one hour before the clocks go back.
	st := Status(s, utc("2026-11-01T05:30:00Z"))
	if st.Active != "night" {
		t.Fatalf("active = %q", st.Active)
	}
	next, err := time.Parse(time.RFC3339, st.NextChange)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(utc("2026-11-01T13:00:00Z")) { // 08:00 EST
		t.Fatalf("next change = %s, want 08:00 EST (13:00 UTC)", next.UTC())
	}
}

func TestApplyScheduleSwapsOnlyRoutingsWithAListForTheWindow(t *testing.T) {
	entries := []config.RoutingEntry{
		{ID: "plan", Models: []string{"day-max"}, Windows: map[string][]string{"night": {"night-max", "day-max"}}},
		{ID: "chat", Models: []string{"day-flash"}},
		{ID: "execute", Models: nil, Windows: map[string][]string{"night": {"night-flash"}}},
	}
	at := utc("2026-10-07T15:00:00Z") // night
	got := ApplySchedule(entries, night(), at)
	if strings.Join(got[0].Models, ",") != "night-max,day-max" {
		t.Errorf("plan = %v", got[0].Models)
	}
	if strings.Join(got[1].Models, ",") != "day-flash" {
		t.Errorf("chat has no night list and must keep its models, got %v", got[1].Models)
	}
	if strings.Join(got[2].Models, ",") != "night-flash" {
		t.Errorf("an automatic routing with a night list must use it, got %v", got[2].Models)
	}
	if strings.Join(entries[0].Models, ",") != "day-max" {
		t.Error("ApplySchedule must not change its input")
	}
	got[0].Models[0] = "edited"
	if entries[0].Windows["night"][0] != "night-max" {
		t.Error("the returned list must not share memory with the window list")
	}
	day := ApplySchedule(entries, night(), utc("2026-10-07T05:00:00Z")) // 13:00
	if strings.Join(day[0].Models, ",") != "day-max" {
		t.Errorf("outside the window plan = %v", day[0].Models)
	}
}

func TestEffectiveRoutingsFollowsTheClockButDeriveRoutingsDoesNot(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Providers = []config.Provider{{Name: "p", Models: []config.ModelEntry{{ID: "day-max"}, {ID: "night-max"}, {ID: "day-flash"}}}}
	cfg.Routing.Schedule = night()
	for i := range cfg.Routing.Routings {
		switch cfg.Routing.Routings[i].ID {
		case "plan":
			cfg.Routing.Routings[i].Models = []string{"day-max"}
			cfg.Routing.Routings[i].Windows = map[string][]string{"night": {"night-max"}}
		default:
			cfg.Routing.Routings[i].Models = []string{"day-flash"}
		}
	}
	planModels := func(entries []config.RoutingEntry) string {
		for _, e := range entries {
			if e.ID == "plan" {
				return strings.Join(e.Models, ",")
			}
		}
		return ""
	}
	clockAt := func(value string) Deps {
		at := utc(value).UnixMilli()
		return Deps{Now: func() int64 { return at }}
	}

	if got := planModels(EffectiveRoutings(&cfg, clockAt("2026-10-07T15:00:00Z"))); got != "night-max" {
		t.Errorf("at night plan = %q", got)
	}
	if got := planModels(EffectiveRoutings(&cfg, clockAt("2026-10-07T05:00:00Z"))); got != "day-max" {
		t.Errorf("by day plan = %q", got)
	}
	// DeriveRoutings feeds `jevonian add`, which saves the result, so it must never
	// copy a window's models into the routing's own list.
	if got := planModels(DeriveRoutings(&cfg, clockAt("2026-10-07T15:00:00Z"))); got != "day-max" {
		t.Errorf("DeriveRoutings at night = %q, want the routing's own models", got)
	}
}
