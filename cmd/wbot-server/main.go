package main

import (
	"context"
	"github.com/wbot-dev/wbot/internal/agent"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/httpapi"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/model"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
	"github.com/wbot-dev/wbot/internal/tool"
	"log"
	"net/http"
)

func main() {
	s, e := config.Load()
	if e != nil {
		log.Fatal(e)
	}
	if _, _, e = config.LoadProfile(s.ProfilePath); e != nil {
		log.Fatal(e)
	}
	st, e := storage.Open(s.DatabasePath, s.DataRoot)
	if e != nil {
		log.Fatal(e)
	}
	defer st.Close()
	mem := memory.New(s.DataRoot + "/memory")
	mainModel := model.New(s.DefaultModel)
	advisor := model.New(s.AdvisorModel)
	perm := permission.New(s, st)
	tools := tool.New(s, st, perm, mem, advisor)
	svc := agent.New(s, st, mainModel, tools, mem)
	if e = svc.Recover(context.Background()); e != nil {
		log.Fatal(e)
	}
	api := httpapi.New(s, st, svc, mem)
	log.Printf("wbot listening on http://%s", s.Addr)
	log.Fatal(http.ListenAndServe(s.Addr, api.Handler()))
}
