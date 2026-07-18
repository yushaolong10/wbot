//go:build wails

package main

import (
	"context"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wbot-dev/wbot/internal/agent"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/httpapi"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/model"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
	"github.com/wbot-dev/wbot/internal/tool"
)

func main() {
	s, e := config.Load()
	if e != nil {
		log.Fatal(e)
	}
	// The Wails asset handler is only mounted inside the local WebView and does
	// not listen on a network socket. Browser/server deployments keep using the
	// configured token, while the trusted local desktop avoids a redundant login.
	s.AuthToken = ""
	if _, _, e = config.LoadProfile(s.ProfilePath); e != nil {
		log.Fatal(e)
	}
	st, e := storage.Open(s.DatabasePath, s.DataRoot)
	if e != nil {
		log.Fatal(e)
	}
	defer st.Close()
	advisor := model.New(s.AdvisorModel)
	mem := memory.New(s.DataRoot+"/memory", memory.WithConfig(memory.ConfigFrom(s.Memory)), memory.WithGenerator(advisor))
	defer mem.Close()
	tools := tool.New(s, st, permission.New(s, st), mem, advisor)
	svc := agent.New(s, st, model.New(s.DefaultModel), tools, mem, advisor)
	if e = svc.Recover(context.Background()); e != nil {
		log.Fatal(e)
	}
	api := httpapi.New(s, st, svc, mem)
	e = wails.Run(&options.App{Title: "wbot", Width: 1280, Height: 820, MinWidth: 900, MinHeight: 600, AssetServer: &assetserver.Options{Handler: api.Handler()}})
	if e != nil {
		log.Fatal(e)
	}
}
