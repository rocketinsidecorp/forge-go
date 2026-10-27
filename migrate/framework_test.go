package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"
	"testing/fstest"
)

// TestFrameworkMigrationsMatchShippedFiles keeps the ownership list in sync
// with every migration file a Forge package ships: each shipped migration is
// listed under its name and a package of its owner, and each listed one is
// shipped. Example applications and test data are not framework packages.
func TestFrameworkMigrationsMatchShippedFiles(t *testing.T) {
	t.Parallel()
	root := os.DirFS("..")
	shippedDirs := map[string]bool{}
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			base := entry.Name()
			if name != "." && (strings.HasPrefix(base, ".") || base == "testdata" || base == "examples" || base == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".up.sql") {
			shippedDirs[path.Dir(name)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shippedDirs) == 0 {
		t.Fatal("found no shipped migration files")
	}

	shipped := map[int64]bool{}
	for dir := range shippedDirs {
		fsys, err := fs.Sub(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		migrations, err := LoadMigrations(fsys)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		pkg := path.Dir(dir) // the package that embeds <pkg>/migrations
		for _, migration := range migrations {
			listed, ok := frameworkMigration(migration.Version)
			if !ok {
				t.Errorf("%s ships %d_%s, which FrameworkMigrations does not list", pkg, migration.Version, migration.Name)
				continue
			}
			if listed.Name != migration.Name || (pkg != listed.Owner && !strings.HasPrefix(pkg, listed.Owner+"/")) {
				t.Errorf("%s ships %d_%s, but FrameworkMigrations lists %+v", pkg, migration.Version, migration.Name, listed)
			}
			shipped[migration.Version] = true
		}
	}
	for _, listed := range FrameworkMigrations() {
		if !shipped[listed.Version] {
			t.Errorf("FrameworkMigrations lists %+v, which no package ships", listed)
		}
	}
}

func TestFrameworkMigrationsAreUniqueAndCopied(t *testing.T) {
	t.Parallel()
	list := FrameworkMigrations()
	seen := map[int64]bool{}
	for i, migration := range list {
		if migration.Name == "" || migration.Owner == "" || seen[migration.Version] {
			t.Fatalf("invalid or duplicate entry %+v", migration)
		}
		if i > 0 && list[i-1].Version >= migration.Version {
			t.Fatalf("not in version order at %+v", migration)
		}
		seen[migration.Version] = true
	}
	list[0].Name = "changed"
	if FrameworkMigrations()[0].Name == "changed" {
		t.Fatal("FrameworkMigrations returned the registry itself")
	}
}

func TestReservedVersions(t *testing.T) {
	t.Parallel()
	cache := fstest.MapFS{"20261019000100_cache.up.sql": {Data: []byte("SELECT 1;")}}
	if _, err := LoadMigrationSets(cache); err != nil {
		t.Fatalf("a framework migration under its own name was refused: %v", err)
	}
	clash := fstest.MapFS{"20261019000100_create_widgets.up.sql": {Data: []byte("SELECT 1;")}}
	_, err := LoadMigrationSets(fstest.MapFS{"20260101000000_create_notes.up.sql": {Data: []byte("SELECT 1;")}}, clash)
	var reserved *ReservedVersionError
	if !errors.As(err, &reserved) || reserved.Version != 20261019000100 || reserved.Name != "create_widgets" || reserved.Reserved.Owner != "cache" {
		t.Fatalf("want *ReservedVersionError, got %v", err)
	}
	if !strings.Contains(err.Error(), "reserved for the cache migration of the Forge cache package") {
		t.Fatalf("message: %v", err)
	}
}

func TestFrameworkMigrationOnlyErrorNamesTheWayOut(t *testing.T) {
	t.Parallel()
	identity, _ := frameworkMigration(20260927090000)
	err := error(&FrameworkMigrationOnlyError{Newest: identity})
	for _, want := range []string{"20260927090000_identity", "from the auth package", "DownFramework", "never reverts"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q lacks %q", err, want)
		}
	}
}

// Superseded checksums are accepted only for the framework migration they
// belong to, recorded under its own name, and only while the build carries
// exactly the pinned current script; they never equal it: an entry for a
// migration whose text did not change, one outside the registry, or a stale
// pin is a mistake.
func TestSupersededChecksumsBelongToShippedFrameworkMigrations(t *testing.T) {
	t.Parallel()
	shipped, err := LoadMigrations(os.DirFS("../auth/postgres/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	byVersion := map[int64]Migration{}
	for _, migration := range shipped {
		byVersion[migration.Version] = migration
	}
	for version, superseded := range supersededChecksums {
		framework, ok := frameworkMigration(version)
		if !ok {
			t.Errorf("%d has superseded checksums but is no framework migration", version)
			continue
		}
		migration, ok := byVersion[version]
		if !ok {
			t.Errorf("%d_%s is not shipped by auth/postgres", version, framework.Name)
			continue
		}
		if superseded.current != migration.Checksum {
			t.Errorf("%d_%s: the pinned current checksum is not the shipped script's (%s)", version, framework.Name, migration.Checksum)
		}
		if len(superseded.old) == 0 {
			t.Errorf("%d_%s lists no earlier checksum", version, framework.Name)
		}
		for _, checksum := range superseded.old {
			if checksum == migration.Checksum {
				t.Errorf("%d_%s lists its current checksum as superseded", version, framework.Name)
			}
			if !checksumMatches(migration, framework.Name, checksum) {
				t.Errorf("%d_%s: the superseded checksum is not accepted", version, framework.Name)
			}
			if checksumMatches(migration, "renamed", checksum) {
				t.Errorf("%d_%s: a row recorded under another name is accepted", version, framework.Name)
			}
			application := migration
			application.Version = 20990101000000
			if checksumMatches(application, framework.Name, checksum) {
				t.Errorf("%d_%s: the checksum is accepted for an application migration", version, framework.Name)
			}
			// A build whose script was tampered with matches neither the
			// row nor the pin: the earlier checksum no longer excuses it.
			tampered := migration
			tampered.Up.SQL += "\nDROP TABLE forge_users;"
			sum := sha256.Sum256([]byte(tampered.Up.SQL))
			tampered.Checksum = hex.EncodeToString(sum[:])
			if checksumMatches(tampered, framework.Name, checksum) {
				t.Errorf("%d_%s: an earlier checksum excuses a tampered build", version, framework.Name)
			}
		}
		if checksumMatches(migration, framework.Name, strings.Repeat("0", 64)) {
			t.Errorf("%d_%s accepts an arbitrary checksum", version, framework.Name)
		}
	}
}
