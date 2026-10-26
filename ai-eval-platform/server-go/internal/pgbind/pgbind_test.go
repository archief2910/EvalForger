package pgbind

import "testing"

func TestRebind(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no placeholders", `SELECT 1`, `SELECT 1`},
		{"simple", `SELECT * FROM t WHERE a=? AND b=?`, `SELECT * FROM t WHERE a=$1 AND b=$2`},
		{"insert", `INSERT INTO t(a,b) VALUES (?,?)`, `INSERT INTO t(a,b) VALUES ($1,$2)`},
		{"literal question mark", `SELECT * FROM t WHERE note='why?' AND a=?`, `SELECT * FROM t WHERE note='why?' AND a=$1`},
		{"escaped quote", `SELECT 'it''s ?' , ?`, `SELECT 'it''s ?' , $1`},
		{"quoted identifier", `SELECT "?" FROM t WHERE a=?`, `SELECT "?" FROM t WHERE a=$1`},
		{"line comment", "SELECT 1 -- what?\nWHERE a=?", "SELECT 1 -- what?\nWHERE a=$1"},
		{"block comment", `SELECT 1 /* ? */ WHERE a=?`, `SELECT 1 /* ? */ WHERE a=$1`},
		{"nested block comment", `SELECT /* a /* ? */ b */ ?`, `SELECT /* a /* ? */ b */ $1`},
		{"many", `VALUES (?,?,?,?,?,?,?,?,?,?,?)`, `VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`},
		{"unterminated literal", `SELECT '?`, `SELECT '?`},
	}
	for _, c := range cases {
		if got := Rebind(c.in); got != c.want {
			t.Errorf("%s: Rebind(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// A question mark that is already numbered must not be double-converted.
func TestRebindKeepsNumbers(t *testing.T) {
	if got := Rebind(`SELECT $1, ?`); got != `SELECT $1, $1` {
		t.Fatalf("got %q", got)
	}
}
