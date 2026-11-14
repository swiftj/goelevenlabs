// Command goelevenlabs is an MCP server (stdio) and CLI for producing finished,
// multi-voice audio scenes with music and sound effects using ElevenLabs.
//
//	goelevenlabs                     run the MCP server on stdio (Claude Desktop / Claude Code)
//	goelevenlabs render scene.txt    render a screenplay script (or scene .json spec)
//	goelevenlabs render -dry s.txt   validate and resolve voices only
//	goelevenlabs status              plan, credits, cache
//	goelevenlabs version
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftj/goelevenlabs/internal/scene"
	"github.com/swiftj/goelevenlabs/internal/server"
)

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr) // stdout is the MCP channel
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "version", "-v", "--version":
		fmt.Println("goelevenlabs", server.Version)
		return
	case "help", "-h", "--help":
		fmt.Println(strings.TrimSpace(usage))
		return
	}
	app, err := server.NewApp()
	if err != nil {
		log.Fatal(err)
	}
	switch cmd {
	case "", "serve", "mcp":
		if err := app.New().Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
	case "render":
		fs := flag.NewFlagSet("render", flag.ExitOnError)
		dry := fs.Bool("dry", false, "validate only")
		out := fs.String("o", "", "output name (no extension)")
		dir := fs.String("dir", "", "output directory")
		wav := fs.Bool("wav", false, "write WAV instead of MP3")
		stems := fs.Bool("stems", false, "also write stems")
		fresh := fs.Bool("fresh", false, "ignore cache")
		_ = fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			log.Fatal("usage: goelevenlabs render [-dry] [-o name] [-dir path] [-wav] [-stems] [-fresh] scene.txt|scene.json")
		}
		b, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		var sp scene.Spec
		if strings.EqualFold(filepath.Ext(fs.Arg(0)), ".json") {
			if err := json.Unmarshal(b, &sp); err != nil {
				log.Fatal(err)
			}
		} else {
			sp.Script = string(b)
			sp.Filename = strings.TrimSuffix(filepath.Base(fs.Arg(0)), filepath.Ext(fs.Arg(0)))
		}
		if *out != "" {
			sp.Filename = *out
		}
		if *dir != "" {
			sp.OutputDir = *dir
		}
		if *wav {
			sp.Format = "wav"
		}
		sp.DryRun = sp.DryRun || *dry
		sp.Stems = sp.Stems || *stems
		sp.Fresh = sp.Fresh || *fresh
		res, err := scene.Render(ctx, app.Env(func(msg string, d, t int) { log.Printf("[%d/%d] %s", d, t, msg) }), sp)
		if err != nil {
			log.Fatal(err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	case "status":
		if sub, err := app.Client.Subscription(ctx); err != nil {
			fmt.Printf("credits: unavailable (%v)\n", err)
		} else {
			fmt.Printf("tier %s: %d / %d credits used\n", sub.Tier, sub.CharacterCount, sub.CharacterLimit)
		}
		files, bytes := app.Client.CacheStats()
		fmt.Printf("cache: %d files, %.1f MB in %s\noutput: %s\n", files, float64(bytes)/1e6, app.Cfg.CacheDir, app.Cfg.OutputDir)
	default:
		log.Fatalf("unknown command %q\n\n%s", cmd, usage)
	}
}

const usage = `
goelevenlabs — ElevenLabs scene production (MCP server + CLI)

  goelevenlabs                 MCP server on stdio
  goelevenlabs render FILE     render a script (.txt) or scene spec (.json)
      -dry -o NAME -dir PATH -wav -stems -fresh
  goelevenlabs status          plan, credits and cache
  goelevenlabs version

API key: $ELEVENLABS_API_KEY, ~/.config/goelevenlabs/config.json {"api_key": "..."},
or the ElevenLabs Player extension already configured in Claude Desktop.
`
