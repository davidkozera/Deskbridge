// Package connfile reads and writes the file that saved connections are
// exported to: a CSV file with one connection per row, which opens in any
// spreadsheet program. It holds passwords as plain text, so that it can be
// loaded on another PC or another Windows account.
package connfile

import (
	"bytes"
	"encoding/csv"
	"errors"
	"strconv"
	"strings"

	"deskbridge/internal/proto"
)

// Display values other than a display's index (counted from 0).
const (
	DisplayNotSet = -2
	DisplayAll    = -1
)

// MaxConnections bounds how many rows are read from a file.
const MaxConnections = 50

// Connection is one row.
type Connection struct {
	Name, Host, Port, Password string
	Width, Height              int // 0, 0: leave the remote display's resolution as it is
	Display                    int // DisplayNotSet, DisplayAll, or a display's index
}

var header = []string{"Name", "Computer", "Port", "Password", "Resolution", "Display"}

// Encode writes the connections as CSV, with a header row. It starts with
// a byte-order mark so that spreadsheet programs read names with accents
// correctly.
func Encode(list []Connection) []byte {
	var b bytes.Buffer
	b.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&b)
	w.UseCRLF = true
	w.Write(header)
	for _, c := range list {
		res, display := "", ""
		if c.Width > 0 && c.Height > 0 {
			res = strconv.Itoa(c.Width) + "x" + strconv.Itoa(c.Height)
		}
		switch {
		case c.Display == DisplayAll:
			display = "all"
		case c.Display >= 0:
			display = strconv.Itoa(c.Display + 1)
		}
		w.Write([]string{c.Name, c.Host, c.Port, c.Password, res, display})
	}
	w.Flush()
	return b.Bytes()
}

// clean removes what cannot be part of a name or a computer's address.
func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '|' || r < ' ' || r == 0x7F {
			return -1
		}
		return r
	}, s))
}

// Decode reads a file written by Encode, or one made by hand with the same
// columns. Rows without a computer are left out; skipped says how many.
// The header row is optional, and columns after the second may be missing.
func Decode(data []byte) (list []Connection, skipped int, err error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // rows may be of different lengths
	r.TrimLeadingSpace = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, 0, errors.New("it is not a CSV file Deskbridge can read")
	}
	seen := map[string]bool{}
	for i, row := range rows {
		field := func(n int) string {
			if n < len(row) {
				return row[n]
			}
			return ""
		}
		if i == 0 && strings.EqualFold(strings.TrimSpace(field(0)), header[0]) && strings.EqualFold(strings.TrimSpace(field(1)), header[1]) {
			continue
		}
		if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
			continue // an empty line
		}
		c := Connection{Name: clean(field(0)), Host: clean(field(1)), Port: strings.TrimSpace(field(2)), Password: field(3), Display: DisplayNotSet}
		if c.Host == "" || strings.ContainsAny(c.Host, " \t") {
			skipped++
			continue
		}
		if c.Name == "" {
			c.Name = c.Host
		}
		if seen[strings.ToLower(c.Name)] {
			skipped++
			continue
		}
		if n, err := strconv.Atoi(c.Port); err != nil || n <= 0 || n > 65535 {
			c.Port = proto.DefaultPort
		}
		if strings.ContainsAny(c.Password, "\r\n") || len(c.Password) > 256 {
			c.Password = ""
		}
		if w, h, ok := strings.Cut(strings.ToLower(strings.ReplaceAll(field(4), " ", "")), "x"); ok {
			wn, err1 := strconv.Atoi(w)
			hn, err2 := strconv.Atoi(h)
			if err1 == nil && err2 == nil && wn >= 320 && hn >= 200 && wn <= 65535 && hn <= 65535 {
				c.Width, c.Height = wn, hn
			}
		}
		switch d := strings.ToLower(strings.TrimSpace(field(5))); {
		case d == "all":
			c.Display = DisplayAll
		case d != "":
			if n, err := strconv.Atoi(d); err == nil && n >= 1 && n <= 16 {
				c.Display = n - 1
			}
		}
		if len(list) >= MaxConnections {
			skipped++
			continue
		}
		seen[strings.ToLower(c.Name)] = true
		list = append(list, c)
	}
	if len(list) == 0 && skipped == 0 {
		return nil, 0, errors.New("it holds no connections")
	}
	return list, skipped, nil
}
