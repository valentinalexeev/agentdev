package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// The outbox (#242) is an append-only JSON-lines log of every reported event, next to the state file. It makes
// delivery durable: printing to stdout is best effort (a wrapper or an agent session can miss it), the outbox is not.
// A reader acknowledges with an offset file (<state>.ack, the byte offset it has read up to), so a restarted
// monitor or a resumed agent session replays exactly what was not acknowledged.

// EvGap: the monitor did not poll for longer than -gap-after. Always an owner-level line.
const EvGap = "gap"

type outboxLine struct {
	At string `json:"at"` // the poll time (snapshot At)
	Event
}

func outboxPath(state string) string { return strings.TrimSuffix(state, ".json") + ".events.jsonl" }
func ackPath(state string) string    { return strings.TrimSuffix(state, ".json") + ".ack" }

func appendOutbox(path, at string, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range events {
		if err := enc.Encode(outboxLine{At: at, Event: e}); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readAck(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("ack file %s: not a byte offset", path)
	}
	return n, nil
}

// printPending prints the owner events (and gaps) after the ack offset, one text line each, then the count. With ack
// it moves the offset to the end of what it read. Events by others stay in the outbox but are not printed here.
func printPending(w io.Writer, state string, ack bool) error {
	off, err := readAck(ackPath(state))
	if err != nil {
		return err
	}
	f, err := os.Open(outboxPath(state))
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(w, "pending owner events: 0")
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if off > st.Size() {
		off = 0 // outbox was replaced: read it again from the start
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(f)
	n, end := 0, off
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			end += int64(len(line))
			var l outboxLine
			if json.Unmarshal(line, &l) == nil && l.Owner {
				fmt.Fprintf(w, "%s * %-16s %s\n", l.At, l.Kind, l.Text)
				n++
			}
		}
		if err != nil {
			break // EOF; a half-written last line is left for the next read
		}
	}
	fmt.Fprintf(w, "pending owner events: %d\n", n)
	if ack {
		return os.WriteFile(ackPath(state), []byte(strconv.FormatInt(end, 10)+"\n"), 0o600)
	}
	return nil
}
