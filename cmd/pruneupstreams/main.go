// Command pruneupstreams removes upstreams from the config by ID keep-list
// and/or by model presence (canonical name, alias canonical key, or raw alias
// base name). It rewrites the config in the same canonical form aisense itself
// persists. Maintenance utility.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"aisense/internal/config"
	"aisense/internal/modelalias"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	keepFile := flag.String("keep", "", "file with upstream IDs to keep, one per line")
	removeModels := flag.String("remove-models", "", "comma-separated model names; removes upstreams whose catalog or aliases contain any of them (exact base-name match)")
	removeIDs := flag.String("remove-ids", "", "comma-separated upstream IDs to remove")
	flag.Parse()
	if *keepFile == "" && *removeModels == "" && *removeIDs == "" {
		fmt.Println("-keep, -remove-models or -remove-ids is required")
		os.Exit(1)
	}
	dropID := map[string]bool{}
	for _, id := range strings.Split(*removeIDs, ",") {
		if id = strings.TrimSpace(id); id != "" {
			dropID[id] = true
		}
	}
	keep := map[string]bool{}
	if *keepFile != "" {
		raw, err := os.ReadFile(*keepFile)
		if err != nil {
			panic(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if id := strings.TrimSpace(line); id != "" {
				keep[id] = true
			}
		}
	}
	dropModel := map[string]bool{}
	for _, m := range strings.Split(*removeModels, ",") {
		if m = strings.TrimSpace(m); m != "" {
			dropModel[strings.ToLower(m)] = true
		}
	}
	data, err := config.ReadFile(*cfgPath)
	if err != nil {
		panic(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		panic(err)
	}
	upstreamHasDroppedModel := func(up *config.Upstream) bool {
		for _, m := range up.Models {
			if dropModel[strings.ToLower(modelalias.BaseName(m))] {
				return true
			}
		}
		for canonical, raws := range up.ModelAliases {
			if dropModel[strings.ToLower(modelalias.BaseName(canonical))] {
				return true
			}
			for _, raw := range raws {
				if dropModel[strings.ToLower(modelalias.BaseName(raw))] {
					return true
				}
			}
		}
		return false
	}
	out := make([]*config.Upstream, 0, len(cfg.Upstreams))
	kept, removedKeep, removedModels := 0, 0, 0
	for _, up := range cfg.Upstreams {
		if up == nil {
			removedKeep++
			continue
		}
		if len(dropID) > 0 && dropID[up.ID] {
			removedKeep++
			fmt.Println("removed (id match):", up.ID, up.BaseURL)
			continue
		}
		if len(keep) > 0 && !keep[up.ID] {
			removedKeep++
			continue
		}
		if len(dropModel) > 0 && upstreamHasDroppedModel(up) {
			removedModels++
			fmt.Println("removed (model match):", up.ID, up.BaseURL)
			continue
		}
		out = append(out, up)
		kept++
	}
	cfg.Upstreams = out
	nb, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		panic(err)
	}
	tmp := *cfgPath + ".prune.tmp"
	if err := config.WriteFile(tmp, nb); err != nil {
		panic(err)
	}
	if err := os.Rename(tmp, *cfgPath); err != nil {
		panic(err)
	}
	fmt.Printf("kept %d, removed by keep-list %d, removed by model match %d\n", kept, removedKeep, removedModels)
}
