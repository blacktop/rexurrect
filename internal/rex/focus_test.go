package rex

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/blacktop/rexurrect/internal/snapshot"
)

// One window with two tiled panes and a floating pane; the focus and zoom
// fields come from the environment so each shape Rex reports can be served.
const focusRexScript = `
if [ "$1" != --server ] || [ "$2" != unix:///private/tmp/rex-client-test-unused ] || [ "$3" != --autostart=false ]; then exit 2; fi
method=$8
if [ "$6" = whoami ]; then method=whoami; fi
case "$method" in
  whoami) printf '%s\n' '{"host_id":"host","instance_id":"instance","rex_version":"version"}' ;;
  server.version) printf '%s\n' '{"rex_version":"version","commit":"commit"}' ;;
  session.list) printf '%s\n' '{"sessions":[{"session_id":"session"}]}' ;;
  session.view)
    printf '{"session_id":"session","revision":1,"label":"saved","active_window_id":"window","windows":[{"window_id":"window","label":"shell"%s%s,"layers":[{"layer_id":"layer","kind":"tiled","layout":{"split":{"split_id":"split","direction":"horizontal","ratio":0.5,"before":{"block_id":"left"},"after":{"block_id":"right"}}},"blocks":[{"block_id":"left","label":"left","flavor":"com.superlogical.terminal.shell"},{"block_id":"right","label":"right","flavor":"com.superlogical.terminal.shell"}]},{"layer_id":"float-layer","kind":"floating","layout":{"block_id":"float"},"blocks":[{"block_id":"float","label":"float","flavor":"com.superlogical.terminal.shell"}]}]}]}\n' "$FOCUS_FIELD" "$ZOOM_FIELD" ;;
  session.list_blocks) printf '%s\n' '{"blocks":[{"block_id":"left","label":"left","flavor":"com.superlogical.terminal.shell","placement":"placed","window_id":"window"},{"block_id":"right","label":"right","flavor":"com.superlogical.terminal.shell","placement":"placed","window_id":"window"},{"block_id":"float","label":"float","flavor":"com.superlogical.terminal.shell","placement":"placed","window_id":"window"}]}' ;;
  com.superlogical.terminal.pwd) /bin/cat "$ROOT/pwd.json" ;;
  com.superlogical.terminal.process) /bin/cat "$ROOT/process.json" ;;
  *) printf 'unexpected method: %s\n' "$method" >&2; exit 1 ;;
esac
`

// Rex focuses and zooms floating panes, and leaves the focus out of a window
// that has none. Each of those once failed the whole save with "invalid focus
// or zoom"; now the window is saved focused on its first tiled pane and the
// report says what Rex reported.
func TestCaptureSubstitutesFocusOutsideTiledLayer(t *testing.T) {
	root := t.TempDir()
	cwd, err := snapshot.CanonicalCWD(root)
	if err != nil {
		t.Fatal(err)
	}
	writeLiveCWDResponses(t, root, cwd, "", "")
	field := func(name, id string) string {
		if id == "" {
			return ""
		}
		return fmt.Sprintf(`,%q:%q`, name, id)
	}
	sub := func(focus, zoom, replacement snapshot.SourceID) []snapshot.FocusSubstitution {
		return []snapshot.FocusSubstitution{{SessionID: "session", WindowID: "window", Focus: focus, Zoom: zoom, Replacement: replacement}}
	}
	cases := []struct {
		name, focus, zoom   string
		wantFocus, wantZoom snapshot.SourceID
		want                []snapshot.FocusSubstitution
	}{
		{"tiled focus and zoom kept", "right", "right", "right", "right", nil},
		{"tiled focus kept", "right", "", "right", "", nil},
		{"floating focus", "float", "", "left", "", sub("float", "", "left")},
		{"floating focus and zoom", "float", "float", "left", "", sub("float", "float", "left")},
		{"no focus", "", "", "left", "", sub("", "", "left")},
		{"floating zoom under tiled focus", "right", "float", "right", "", sub("right", "float", "right")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := scriptClient(t, focusRexScript, "ROOT="+root, "FOCUS_FIELD="+field("focused_block_id", tc.focus), "ZOOM_FIELD="+field("zoomed_block_id", tc.zoom))
			saved, report, err := c.Capture(t.Context(), []snapshot.LiveID{"session"})
			if err != nil {
				t.Fatal(err)
			}
			w := saved.Sessions[0].Windows[0]
			if w.Focus != tc.wantFocus || w.Zoom != tc.wantZoom || !reflect.DeepEqual(report.FocusSubstitutions, tc.want) {
				t.Fatalf("focus %q zoom %q substitutions %+v", w.Focus, w.Zoom, report.FocusSubstitutions)
			}
			if len(report.Unsupported) != 1 || report.Unsupported[0].LayerID != "float-layer" {
				t.Fatalf("floating layer not reported: %+v", report.Unsupported)
			}
		})
	}
}
