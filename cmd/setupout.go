package cmd

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// surfaceURL is where a client reaches svc listening on addr.
func surfaceURL(svc, addr string) string {
	if svc == "mcp" {
		return "http://" + addr + "/mcp"
	}
	return "http://" + addr
}

// surfaceExample is how to call svc at url, the token read from
// $HEO_TOKEN: never the token itself.
func surfaceExample(svc, url string) (title, body string) {
	switch svc {
	case "api":
		return "REST: discovery, then a read", strings.Join([]string{
			`curl -s -H "Authorization: Bearer $HEO_TOKEN" ` + url + `/v1/commands`,
			`curl -s -H "Authorization: Bearer $HEO_TOKEN" '` + url + `/v1/commands/config/get?arg=runtime.tenant'`,
		}, "\n")
	case "rpc":
		return "gRPC (Connect JSON)", `curl -s ` + url + `/cmdsurface.v1.Commands/Invoke \` + "\n" +
			`  -H "Authorization: Bearer $HEO_TOKEN" -H 'Content-Type: application/json' \` + "\n" +
			`  -d '{"path":["config","get"],"args":["runtime.tenant"]}'`
	case "mcp":
		return "MCP client configuration", `{"mcpServers": {"heo": {"type": "http", "url": "` + url +
			`", "headers": {"Authorization": "Bearer ${HEO_TOKEN}"}}}}`
	}
	return "", ""
}

func writeSetup(w io.Writer, r setupResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	state := "unchanged"
	if r.Changed {
		state = "updated"
	}
	rows := [][2]string{{"profile", r.Profile}, {"config", r.Config + " (" + state + ")"}}
	for _, s := range r.Surfaces {
		v := "off"
		if s.Enabled {
			v = "on  " + s.URL
		}
		if s.Changed {
			v += "  (changed)"
		}
		rows = append(rows, [2]string{s.Name, v})
	}
	if t := r.Token; t != nil {
		rows = append(rows, [2]string{"token", fmt.Sprintf("%s: %s in the %s store, acts as %s", t.Action, t.Ref, t.Backend, t.Subject)})
		if t.Value != "" {
			rows = append(rows, [2]string{"token value", t.Value})
		}
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	var b strings.Builder
	if r.Restart != "" {
		fmt.Fprintf(&b, "\nApply: %s\n", r.Restart)
	}
	var any bool
	for _, s := range r.Surfaces {
		if !s.Enabled {
			continue
		}
		if !any {
			b.WriteString("\nCall with the token in $HEO_TOKEN (heo service setup --show-token prints it):\n")
			any = true
		}
		title, body := surfaceExample(s.Name, s.URL)
		fmt.Fprintf(&b, "\n  %s\n", title)
		for _, line := range strings.Split(body, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
