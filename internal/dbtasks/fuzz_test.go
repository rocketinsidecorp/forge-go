package dbtasks

import (
	"maps"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

// Canaries planted in the fuzzed URLs: errors never echo a URL, and
// Describe never shows a password.
const (
	appCanary  = "Kq7appZ"
	privCanary = "Wm3privP"
)

// canonicalPort is a port from 1 to 65535 without leading zeros.
var canonicalPort = regexp.MustCompile(`^([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])$`)

// tlsFileParams make pgx read files while parsing; the fuzz target leaves
// such URLs to the unit tests rather than reading arbitrary paths.
var tlsFileParams = []string{"sslrootcert", "sslcert", "sslkey", "passfile", "sslpassword"}

// FuzzNewTarget checks the URL rules of forge db: errors and descriptions
// never quote a URL or password, and for an accepted target every consumer
// of the administration URL agrees on the server and database: net/url (which
// Local and the reserved-name check read), pgx, the PG* variables of the
// PostgreSQL tools and the option file of the MySQL tools.
func FuzzNewTarget(f *testing.F) {
	for _, seed := range []struct{ appBefore, appAfter, privBefore, privAfter string }{
		{"postgres://app:", "@db.internal:5433/shop?sslmode=disable", "", ""},
		{"postgres://app:", "@127.0.0.1/shop?sslmode=disable", "postgres://root:", "@localhost/ignored?sslmode=disable"},
		{"postgres://app:", "@db/shop?host=evil", "", ""},
		{"postgres://app:", "@db/shop?sslmode=disable&%20host=evil", "", ""},
		{"postgres://app:", "@db/shop?sslmode=prefer", "", ""},
		{"postgres://app:", "@db/postgres?sslmode=disable", "", ""},
		{"postgres://app:", "@db/shop", "postgres://root:", "@db/x?options=-c%20search_path%3Devil"},
		{"postgres://app:", "@[::1]:5432/shop?sslmode=disable", "", ""},
		{"postgres://app:", "@%2Ftmp/shop?sslmode=disable", "", ""},
		{"postgres://app:", "@db:0/shop", "", ""},
		{"postgres://app:", "@db:99999/shop", "", ""},
		{"mysql://root:", "@db:3306/shop", "", ""},
		{"mysql://root:", "@db/shop?tls=true", "mysql://admin:", `"\#@127.0.0.1/x`},
		{"mysql://", "@db/sh$op", "", ""},
		{"mysql://root:", "@db:99999/shop", "", ""},
		{"mysql://root:", "@db/shop", "mysql://admin:", "@db:065535/x"},
		{"sqlite:storage/", ".db", "", ""},
		{"sqlite:///var/lib/", "/app.db", "", ""},
	} {
		f.Add(seed.appBefore, seed.appAfter, seed.privBefore, seed.privAfter)
	}
	f.Fuzz(func(t *testing.T, appBefore, appAfter, privBefore, privAfter string) {
		appURL := appBefore + appCanary + appAfter
		env := map[string]string{"FORGE_DATABASE_URL": appURL}
		if privBefore != "" || privAfter != "" {
			env[EnvPrivilegedURL] = privBefore + privCanary + privAfter
		}
		target, err := NewTarget("/srv/app", lookupOf(env))
		if err != nil {
			if strings.Contains(err.Error(), appCanary) || strings.Contains(err.Error(), privCanary) {
				t.Fatalf("error quotes a URL: %v", err)
			}
			return
		}
		if target.Adapter == config.AdapterSQLite {
			return
		}
		describe := target.Describe()
		for _, u := range []*url.URL{target.app, target.priv} {
			password, _ := u.User.Password()
			for _, canary := range []string{appCanary, privCanary} {
				if strings.Contains(password, canary) && !strings.Contains(u.Host, canary) && !strings.Contains(target.Name, canary) && strings.Contains(describe, canary) {
					t.Fatalf("Describe shows a password: %q", describe)
				}
			}
		}
		for _, u := range []*url.URL{target.app, target.priv} {
			if port := u.Port(); port != "" && !canonicalPort.MatchString(port) {
				t.Fatalf("accepted port %q: the tools and pgx would read it differently", port)
			}
		}
		if !databaseName.MatchString(target.Name) {
			t.Fatalf("accepted database name %q", target.Name)
		}
		if (target.checkMutable() == nil) == reserved[strings.ToLower(target.Name)] {
			t.Fatalf("checkMutable disagrees with the reserved list for %q", target.Name)
		}
		for _, u := range []*url.URL{target.app, target.priv} {
			for key := range u.Query() {
				for _, param := range locationParams {
					if strings.EqualFold(strings.TrimSpace(key), param) {
						t.Fatalf("accepted location parameter %q", key)
					}
				}
			}
		}
		switch target.Adapter {
		case config.AdapterPostgres:
			checkPostgresAgreement(t, target)
		case config.AdapterMySQL:
			for _, flavor := range []string{"mysql", "mariadb"} {
				checkMySQLOptionFile(t, target, flavor)
			}
		}
	})
}

// checkPostgresAgreement compares what pgx would dial with what Local judged
// and with the PG* variables the vendor tools receive.
func checkPostgresAgreement(t *testing.T, target *Target) {
	t.Helper()
	query := target.priv.Query()
	for _, key := range tlsFileParams {
		if query.Has(key) {
			return
		}
	}
	cfg, err := target.pgConnConfig(target.Name)
	if err != nil {
		if strings.Contains(err.Error(), appCanary) || strings.Contains(err.Error(), privCanary) {
			t.Fatalf("pgConnConfig error quotes a URL: %v", err)
		}
		return
	}
	host := target.priv.Hostname()
	if cfg.Database != target.Name || cfg.Host != host {
		t.Fatalf("pgx dials %q/%q, forge checked %q/%q", cfg.Host, cfg.Database, host, target.Name)
	}
	for _, fallback := range cfg.Fallbacks {
		if fallback.Host != cfg.Host || fallback.Port != cfg.Port {
			t.Fatalf("pgx falls back to %s:%d", fallback.Host, fallback.Port)
		}
	}
	ip := net.ParseIP(cfg.Host)
	if target.Local() && !strings.HasPrefix(cfg.Host, "/") && !strings.EqualFold(cfg.Host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		t.Fatalf("Local() but pgx dials %q", cfg.Host)
	}
	variables := map[string]string{}
	for _, entry := range target.pgEnvironment(nil) {
		name, value, _ := strings.Cut(entry, "=")
		variables[name] = value
	}
	if variables["PGHOST"] != cfg.Host || variables["PGPORT"] != strconv.Itoa(int(cfg.Port)) || variables["PGDATABASE"] != cfg.Database {
		t.Fatalf("tools dial %s:%s/%s, pgx %s:%d/%s", variables["PGHOST"], variables["PGPORT"], variables["PGDATABASE"], cfg.Host, cfg.Port, cfg.Database)
	}
	if strings.Contains(variables["PGHOST"], ",") {
		t.Fatalf("PGHOST %q names several hosts", variables["PGHOST"])
	}
	if mode := query.Get("sslmode"); mode != variables["PGSSLMODE"] {
		t.Fatalf("tools get sslmode %q, the URL says %q", variables["PGSSLMODE"], mode)
	}
}

// checkMySQLOptionFile reads the option file back the way the MySQL and
// MariaDB clients do (mysys: end-of-line comments outside quotes, surrounding
// quotes stripped, backslash escapes) and compares it with the URL.
func checkMySQLOptionFile(t *testing.T, target *Target, flavor string) {
	t.Helper()
	c := credentialsOf(target.priv)
	content, err := mysqlOptionFile(c, target.Name, flavor)
	if err != nil {
		return
	}
	want := map[string]string{"protocol": "tcp", "port": portOr(c.port, "3306"), "database": target.Name}
	for key, value := range map[string]string{"user": c.user, "password": c.password, "host": c.host} {
		if value != "" {
			want[key] = value
		}
	}
	tlsKeys := map[string]bool{"ssl": true, "ssl-verify-server-cert": true, "ssl-mode": true}
	got := map[string]string{}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if lines[0] != "[client]" {
		t.Fatalf("option file starts with %q", lines[0])
	}
	for _, line := range lines[1:] {
		key, value, ok := readOptionLine(line)
		if !ok {
			t.Fatalf("unreadable option line %q", line)
		}
		if tlsKeys[key] {
			continue
		}
		if _, duplicate := got[key]; duplicate {
			t.Fatalf("option %q set twice in %q", key, content)
		}
		got[key] = value
	}
	if !maps.Equal(got, want) {
		t.Fatalf("the client reads %q, want %q (file %q)", got, want, content)
	}
}

// readOptionLine parses one option line like mysys's option file reader.
func readOptionLine(line string) (key, value string, ok bool) {
	quote, escape := byte(0), false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if (c == '\'' || c == '"') && !escape {
			switch quote {
			case 0:
				quote = c
			case c:
				quote = 0
			}
		}
		if quote == 0 && c == '#' {
			line = line[:i]
			break
		}
		escape = quote != 0 && c == '\\' && !escape
	}
	key, value, ok = strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[0] == value[len(value)-1] {
		value = value[1 : len(value)-1]
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i == len(value)-1 {
			b.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 's':
			b.WriteByte(' ')
		case '"', '\'', '\\':
			b.WriteByte(value[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(value[i])
		}
	}
	return key, b.String(), true
}
