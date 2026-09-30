// Command importvalidapis imports "url key | model,model,..." export lines
// into an aisense config file: one upstream per URL with every unique key in
// first-seen order and the union of raw model routes (canonicalized exactly
// like discovery). "<no-auth-needed>" lines become AuthMode none. URLs that
// are already present in the config (config.NormalizeBaseURL match) are
// skipped; all existing upstreams are preserved untouched. The config is
// rewritten atomically (tmp+rename) without starting a config.Manager.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"aisense/internal/admin"
	"aisense/internal/config"
)

func main() {
	file := flag.String("file", "valid-apis-w-models.txt", "path to the url+key+models export file")
	cfgPath := flag.String("config", "config.json", "path to the aisense config file")
	dryRun := flag.Bool("dry-run", false, "report the summary without rewriting the config")
	flag.Parse()
	if err := run(*file, *cfgPath, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(file, cfgPath string, dryRun bool) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	data, err := config.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", cfgPath, err)
	}
	res := admin.ParseImports(string(raw))
	added := admin.MergeImportUpstreams(res, cfg.Upstreams)

	fileURLs := map[string]bool{}
	for _, up := range res.Upstreams {
		if up != nil && up.BaseURL != "" {
			fileURLs[strings.ToLower(config.NormalizeBaseURL(up.BaseURL))] = true
		}
	}
	keys, models := 0, 0
	for _, up := range added {
		keys += len(up.APIKeys)
		models += len(up.Models)
	}
	fmt.Printf("parsed %d entries (%d malformed), %d unique URLs\n", len(res.Upstreams), len(res.Skipped), len(fileURLs))
	for _, up := range added {
		fmt.Printf("  + %s id=%s keys=%d models=%d auth=%s\n", up.BaseURL, up.ID, len(up.APIKeys), len(up.Models), up.AuthMode)
	}
	fmt.Printf("added %d, skipped %d already configured, %d keys, %d models\n",
		len(added), len(fileURLs)-len(added), keys, models)
	if dryRun || len(added) == 0 {
		fmt.Println("config not modified")
		return nil
	}
	cfg.Upstreams = append(cfg.Upstreams, added...)
	out, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteFile(cfgPath, out); err != nil {
		return err
	}
	fmt.Printf("config rewritten: %s (%d upstreams total)\n", cfgPath, len(cfg.Upstreams))
	return nil
}
