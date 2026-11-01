package ports

import (
	"bufio"
	"context"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

type probeResult struct {
	http  bool
	title string
}

const probeTimeout = 5 * time.Second

// probeHTTP sends "GET /" through dial. conclusive is false if nothing came
// back (the server may still be starting); any reply that isn't HTTP means
// the port speaks another protocol.
func probeHTTP(ctx context.Context, dial func(context.Context) (net.Conn, error), port int) (res probeResult, conclusive bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := dial(ctx)
	if err != nil {
		return res, false
	}
	defer conn.Close()
	dl, _ := ctx.Deadline()
	conn.SetDeadline(dl)
	// Host: localhost passes dev servers' DNS rebinding checks (Vite etc.).
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: localhost:%d\r\nUser-Agent: dune-probe\r\nAccept: text/html,*/*\r\nConnection: close\r\n\r\n", port)
	br := bufio.NewReader(conn)
	if _, err := br.Peek(1); err != nil {
		return res, false
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return res, true
	}
	defer resp.Body.Close()
	res.http = true
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/html" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		res.title = extractTitle(body)
	}
	return res, true
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

const maxTitle = 120

func extractTitle(body []byte) string {
	m := titleRE.FindSubmatch(body)
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	if r := []rune(t); len(r) > maxTitle {
		t = string(r[:maxTitle-1]) + "…"
	}
	return t
}

// Programs that run a script named by their arguments.
var runtimeRE = regexp.MustCompile(`^(node|nodejs|bun|deno|python[0-9.]*|ruby|php|perl)$`)

// label names what is listening: the script for interpreters ("node
// .../vite.js" -> "vite", "python3 -m http.server" -> "http.server"), else
// the program.
func label(comm string, args []string) string {
	if len(args) == 0 {
		return comm
	}
	// Programs that rename themselves ("next-server (v15.1.0)") put it all
	// in argv[0].
	prog := comm
	if f := strings.Fields(args[0]); len(f) > 0 {
		prog = path.Base(f[0])
	}
	if !runtimeRE.MatchString(prog) {
		return prog
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "-"):
			continue
		}
		name := path.Base(a)
		for _, ext := range []string{".js", ".mjs", ".cjs", ".ts"} {
			name = strings.TrimSuffix(name, ext)
		}
		return name
	}
	return prog
}
