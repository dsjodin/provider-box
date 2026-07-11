package logs

import "testing"

func TestExtract(t *testing.T) {
	lines := []string{
		`{"time":"2026-07-09T12:00:00.1Z","level":"INFO","msg":"reconcile ok"}`,
		`{"time":"2026-07-09T12:00:01.2Z","level":"ERROR","msg":"reconcile failed","err":"dial tcp: refused"}`,
		`{"time":"2026-07-09T12:00:02Z","level":"WARN","msg":"retrying"}`,
		`{"level":"ERROR+2","msg":"severe"}`,
		`plain text with no level`,
		`traceback: level=error something broke`,
		``,
	}

	got := Extract("dns-sync", lines)
	if len(got) != 3 {
		t.Fatalf("got %d error entries, want 3: %+v", len(got), got)
	}

	if got[0].Level != "ERROR" || got[0].Message != "reconcile failed" {
		t.Errorf("first entry: %+v", got[0])
	}
	if got[0].Time != "2026-07-09T12:00:01Z" {
		t.Errorf("time not normalized: %q", got[0].Time)
	}
	if got[0].Container != "dns-sync" {
		t.Errorf("container = %q", got[0].Container)
	}
	if got[1].Level != "ERROR+2" {
		t.Errorf("ERROR+2 should be treated as error: %+v", got[1])
	}
	if got[2].Message != "traceback: level=error something broke" {
		t.Errorf("non-JSON error line: %+v", got[2])
	}
}

func TestIsErrorLevel(t *testing.T) {
	for _, l := range []string{"ERROR", "error", "ERROR+1", "FATAL", "panic"} {
		if !isErrorLevel(l) {
			t.Errorf("%q should be error level", l)
		}
	}
	for _, l := range []string{"INFO", "WARN", "DEBUG", ""} {
		if isErrorLevel(l) {
			t.Errorf("%q should not be error level", l)
		}
	}
}
