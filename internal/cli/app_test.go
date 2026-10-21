package cli

import (
	"flag"
	"slices"
	"testing"
)

func TestGlobalFlagsAnywhere(t *testing.T) {
	a := &App{Home: "default"}
	rest, noColor := a.globalFlags([]string{"keys", "create", "bot", "--json", "--rpm", "5", "--home=/srv/node", "--no-color", "--", "--json"})
	if !a.JSON || !noColor || a.Home != "/srv/node" {
		t.Fatalf("flags not applied: json=%v noColor=%v home=%q", a.JSON, noColor, a.Home)
	}
	want := []string{"keys", "create", "bot", "--rpm", "5", "--", "--json"}
	if !slices.Equal(rest, want) {
		t.Fatalf("rest = %q, want %q", rest, want)
	}

	a = &App{}
	if rest, _ = a.globalFlags([]string{"status", "--home", "/tmp/x"}); a.Home != "/tmp/x" || !slices.Equal(rest, []string{"status"}) {
		t.Fatalf("--home <dir>: home=%q rest=%q", a.Home, rest)
	}
}

func TestParseFlagsInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	price := fs.String("price", "", "")
	yes := fs.Bool("yes", false, "")
	pos, err := parseFlags(fs, []string{"--yes", "llama-3.3-70b", "--price", "0.5"})
	if err != nil {
		t.Fatal(err)
	}
	if !*yes || *price != "0.5" || !slices.Equal(pos, []string{"llama-3.3-70b"}) {
		t.Fatalf("yes=%v price=%q pos=%q", *yes, *price, pos)
	}
}
