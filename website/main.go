package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.Repo, "repo", "..", "path to the tommy repository root")
	flag.StringVar(&cfg.Out, "out", "../site", "directory to write the site into")
	flag.StringVar(&cfg.Providers, "providers", "",
		"read `tommy providers --json` output from this file instead of running it")
	flag.Parse()

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", flag.Arg(0))
		os.Exit(2)
	}

	site, err := Build(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
	abs, _ := filepath.Abs(cfg.Out)
	fmt.Printf("wrote %d pages to %s\n", len(site.Pages), abs)
	for repoPath, from := range site.Unpublished() {
		fmt.Printf("  not published, linked to GitHub: %s (from %v)\n", repoPath, from)
	}
}
