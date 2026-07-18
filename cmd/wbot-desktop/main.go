// wbot-desktop starts the local runtime and opens the shared Web UI in the
// platform browser. It is intentionally thin; the same HTTP/SSE application
// service is used by local and cloud modes.
package main

import (
	"context"
	"fmt"
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
	"os"
	"os/exec"
	"runtime"
	"time"
)

func main() {
	if remote := os.Getenv("WBOT_REMOTE_URL"); remote != "" {
		open(remote)
		return
	}
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
	mainModel := model.New(s.DefaultModel)
	advisor := model.New(s.AdvisorModel)
	mem := memory.New(s.DataRoot+"/memory", memory.WithConfig(memory.ConfigFrom(s.Memory)), memory.WithGenerator(mainModel))
	defer mem.Close()
	tools := tool.New(s, st, permission.New(s, st), mem, advisor)
	svc := agent.New(s, st, mainModel, tools, mem, mainModel)
	_ = svc.Recover(context.Background())
	api := httpapi.New(s, st, svc, mem)
	url := "http://" + s.Addr
	go func() { time.Sleep(500 * time.Millisecond); open(url) }()
	log.Printf("wbot desktop local runtime: %s", url)
	log.Fatal(http.ListenAndServe(s.Addr, api.Handler()))
}
func open(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	if e := c.Start(); e != nil {
		fmt.Println("Open", url, "in a browser")
	}
}
