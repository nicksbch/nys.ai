package main

import (
	"strings"
	"testing"
	"time"
)

func TestSuggest(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	objs := []object{
		{Key: "uk/crime/2026-08.zip", Modified: day(23)},
		{Key: "uk/crime/2026-07.zip", Modified: day(20)},
		{Key: "uk/crime/2023-08.zip", Modified: day(1)},
		{Key: "uk/crime/extracted/2026-08/2026-08-avon-and-somerset-street.csv", Modified: day(23)},
		{Key: "uk/price-paid/pp-2025.csv", Modified: day(10)},
		{Key: "old/2019-01.zip", Modified: day(2)},
		{Key: "isos/debian.iso", Modified: day(3)},
		{Key: "uk/crime/", Modified: day(1)}, // a folder marker
	}
	got := suggest(objs, "2023-08.zip")
	if len(got) != 2 {
		t.Fatalf("suggestions = %+v", got)
	}
	first := got[0]
	if first.Key != "uk/crime/2023-08.zip" || first.Folder != "uk/crime/" || first.Match != "name pattern" ||
		first.Count != 3 || !first.Exists || strings.Join(first.Similar, ",") != "uk/crime/2026-08.zip,uk/crime/2026-07.zip,uk/crime/2023-08.zip" {
		t.Errorf("first = %+v", first)
	}
	if got[1].Key != "old/2023-08.zip" || got[1].Count != 1 || got[1].Exists {
		t.Errorf("second = %+v", got[1])
	}

	// No name matches: folders with the same extension, marked as weaker.
	got = suggest(objs, "debian-13.iso")
	if len(got) != 1 || got[0].Key != "isos/debian-13.iso" || got[0].Match != "extension" {
		t.Errorf("by extension = %+v", got)
	}
	// Files at the top of the bucket suggest the top.
	got = suggest([]object{{Key: "2024-01.zip"}}, "2023-08.zip")
	if len(got) != 1 || got[0].Key != "2023-08.zip" || got[0].Folder != "" {
		t.Errorf("top = %+v", got)
	}
	// Nothing alike.
	if got := suggest(objs, "report.pdf"); len(got) != 0 {
		t.Errorf("nothing alike = %+v", got)
	}
}

func TestShape(t *testing.T) {
	for in, want := range map[string]string{"2023-08.zip": "####-##.zip", "PP-2025.CSV": "pp-####.csv", "debian.iso": "debian.iso"} {
		if got := shape(in); got != want {
			t.Errorf("shape(%q) = %q, want %q", in, got, want)
		}
	}
}
