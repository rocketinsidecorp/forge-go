package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/internal/terminal"
)

const (
	commandUsage  = "usage: forge credentials <init|edit|show|rotate-key> [--environment E] [flags]"
	redactedValue = "[REDACTED]"
	// gitignoreEntry keeps credential keys out of version control; the
	// encrypted .json.enc files are meant to be committed.
	gitignoreEntry    = "/config/credentials/*.key"
	dockerignoreEntry = "config/credentials/*.key"
)

// credentialsContext carries the process state the subcommands use, so tests
// can substitute it.
type credentialsContext struct {
	dir            string // credentials directory
	gitignore      string // path of .gitignore
	dockerignore   string // path of .dockerignore, updated only if it exists
	lookup         func(string) (string, bool)
	stdout, stderr io.Writer
	stdoutIsTTY    func() bool
	editor         editorRunner
}

func runCredentials(args []string, stdout, stderr io.Writer) error {
	ctx := credentialsContext{
		dir:          credentials.DefaultDir,
		gitignore:    ".gitignore",
		dockerignore: ".dockerignore",
		lookup:       os.LookupEnv,
		stdout:       stdout,
		stderr:       stderr,
		stdoutIsTTY: func() bool {
			file, ok := stdout.(*os.File)
			return ok && terminal.IsTerminal(file)
		},
		editor: runEditorProcess,
	}
	return ctx.run(args)
}

func (c *credentialsContext) run(args []string) error {
	if len(args) == 0 {
		return errors.New(commandUsage)
	}
	flags := flag.NewFlagSet("credentials "+args[0], flag.ContinueOnError)
	flags.SetOutput(c.stderr)
	defaultEnv := "development"
	if value, ok := c.lookup("FORGE_ENV"); ok && value != "" {
		defaultEnv = value
	}
	environment := flags.String("environment", defaultEnv, "environment whose credentials to use (default: $FORGE_ENV or development)")
	flags.StringVar(environment, "e", defaultEnv, "shorthand for --environment")
	var reveal, keepOld, printKey, writeKeyFile bool
	switch args[0] {
	case "show":
		flags.BoolVar(&reveal, "reveal", false, "print the values (interactive terminal only)")
	case "rotate-key":
		flags.BoolVar(&keepOld, "keep-old", false, "keep the previous keys readable in the new key list (migration window)")
		flags.BoolVar(&printKey, "print-key", false, "print the new key list to standard output")
		flags.BoolVar(&writeKeyFile, "write-key-file", false, "store the new keys in the environment's key file even when the key came from FORGE_MASTER_KEY")
	case "init", "edit":
	default:
		return fmt.Errorf("unknown credentials command %q\n%s", args[0], commandUsage)
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := credentials.ValidateEnvironment(*environment); err != nil {
		return err
	}
	switch args[0] {
	case "init":
		return c.init(*environment)
	case "edit":
		return c.edit(*environment)
	case "show":
		return c.show(*environment, reveal)
	default:
		return c.rotate(*environment, keepOld, printKey, writeKeyFile)
	}
}

func (c *credentialsContext) init(env string) error {
	if _, err := credentials.ReadFile(c.dir, env); err == nil {
		return fmt.Errorf("%s already exists; use `forge credentials edit --environment %s`", filepath.Join(c.dir, credentials.FileName(env)), env)
	} else if !errors.Is(err, credentials.ErrNotFound) {
		return err
	}
	created, err := credentials.Init(c.dir, env)
	for _, path := range created {
		fmt.Fprintf(c.stdout, "  create  %s\n", path)
	}
	if err != nil {
		return err
	}
	if err := ensureLine(c.gitignore, gitignoreEntry, true); err != nil {
		return fmt.Errorf("update .gitignore (add %q yourself before committing): %w", gitignoreEntry, err)
	}
	if err := ensureLine(c.dockerignore, dockerignoreEntry, false); err != nil {
		return fmt.Errorf("update .dockerignore (add %q yourself): %w", dockerignoreEntry, err)
	}
	fmt.Fprintf(c.stdout, "  ignore  %s in .gitignore\n", gitignoreEntry)
	keyPath := filepath.Join(c.dir, credentials.KeyFileName(env))
	_, err = fmt.Fprintf(c.stdout, `
The key in %[1]s is the only way to read %[2]s.
Back it up in a password manager or secret store; it is never committed.
Deploy with:  FORGE_MASTER_KEY=$(cat %[1]s)   (see docs/CREDENTIALS.md)
Next:         forge credentials edit --environment %[3]s
`, keyPath, filepath.Join(c.dir, credentials.FileName(env)), env)
	return err
}

// ensureLine appends line to the ignore file at path unless an equivalent
// entry exists. With create the file is created when missing.
func ensureLine(path, line string, create bool) error {
	data, err := os.ReadFile(path) // #nosec G304 -- fixed ignore-file names in the working directory
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil
		}
		return os.WriteFile(path, []byte(line+"\n"), 0o600)
	}
	if err != nil {
		return err
	}
	for _, existing := range strings.Split(string(data), "\n") {
		existing = strings.TrimSpace(existing)
		if existing == line || existing == strings.TrimPrefix(line, "/") || existing == "*.key" || existing == "/config/credentials/" || existing == "config/credentials/" {
			return nil
		}
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- fixed ignore-file names in the working directory
	if err != nil {
		return err
	}
	prefix := ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n"
	}
	if _, err := file.WriteString(prefix + "# Credential keys: never commit.\n" + line + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// readPlaintext decrypts the credentials of env and returns them with the
// keys and the encrypted file as read (for lost-update detection).
func (c *credentialsContext) readPlaintext(env string) (plaintext []byte, keys []encryption.Key, source credentials.KeySource, file []byte, err error) {
	keys, source, err = credentials.LoadKeys(c.dir, env, c.lookup)
	if errors.Is(err, credentials.ErrKeyMissing) {
		return nil, nil, source, nil, fmt.Errorf("%w; create the credentials with `forge credentials init --environment %s` or provide the key", err, env)
	}
	if err != nil {
		return nil, nil, source, nil, err
	}
	file, err = credentials.ReadFile(c.dir, env)
	if errors.Is(err, credentials.ErrNotFound) {
		return nil, keys, source, nil, fmt.Errorf("%s not found; run `forge credentials init --environment %s`", filepath.Join(c.dir, credentials.FileName(env)), env)
	}
	if err != nil {
		return nil, nil, source, nil, err
	}
	plaintext, err = credentials.Unseal(env, file, keys)
	if err != nil {
		return nil, nil, source, nil, err
	}
	return plaintext, keys, source, file, nil
}

func (c *credentialsContext) show(env string, reveal bool) error {
	if reveal && !c.stdoutIsTTY() {
		return errors.New("--reveal prints secrets and needs an interactive terminal; output to a pipe or file is refused (use `forge credentials edit` to change values)")
	}
	plaintext, _, _, _, err := c.readPlaintext(env)
	if err != nil {
		return err
	}
	defer clear(plaintext)
	var out []byte
	if reveal {
		var indented bytes.Buffer
		if err := json.Indent(&indented, plaintext, "", "  "); err != nil {
			return err
		}
		out = append(indented.Bytes(), '\n')
	} else if out, err = redactDocument(plaintext); err != nil {
		return err
	}
	_, err = c.stdout.Write(out)
	return err
}

// redactDocument keeps the structure (keys, nesting, array lengths) and
// replaces every value.
func redactDocument(plaintext []byte) ([]byte, error) {
	var doc any
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return nil, errors.New("credentials document is not valid JSON")
	}
	out, err := json.MarshalIndent(redactValue(doc), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func redactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			v[key] = redactValue(item)
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = redactValue(item)
		}
		return v
	}
	return redactedValue
}

func (c *credentialsContext) rotate(env string, keepOld, printKey, writeKeyFile bool) error {
	plaintext, keys, source, file, err := c.readPlaintext(env)
	if err != nil {
		return err
	}
	defer clear(plaintext)
	toFile := !source.Environment || writeKeyFile
	if !toFile && !printKey {
		return errors.New("the current key comes from FORGE_MASTER_KEY, so there is no key file to update: pass --print-key (to store the new key in your secret manager) or --write-key-file")
	}
	material, err := encryption.GenerateKey()
	if err != nil {
		return err
	}
	fresh, err := credentials.NewKey(material)
	clear(material)
	if err != nil {
		return err
	}
	withOld := append([]encryption.Key{fresh}, keys...)
	final := withOld
	if !keepOld {
		final = []encryption.Key{fresh}
	}
	sealed, err := credentials.Seal(env, plaintext, fresh)
	if err != nil {
		return err
	}
	if printKey {
		// Before any file changes: if this fails, nothing was modified.
		if _, err := fmt.Fprintln(c.stdout, joinKeys(final)); err != nil {
			return err
		}
	}
	// Crash safety: the key file first gains the new key while still holding
	// the old ones, then the encrypted file changes, and only then are the old
	// keys dropped. At every instant the file on disk is readable with the key
	// file on disk.
	if toFile {
		if err := credentials.WriteKeyFile(c.dir, env, withOld, true); err != nil {
			return err
		}
	}
	current, err := credentials.ReadFile(c.dir, env)
	if err != nil || !bytes.Equal(current, file) {
		return errors.New("the credentials file changed during rotation; nothing was re-encrypted, run the command again")
	}
	if err := credentials.WriteFile(c.dir, env, sealed, true); err != nil {
		return err
	}
	if toFile && !keepOld {
		if err := credentials.WriteKeyFile(c.dir, env, final, true); err != nil {
			return fmt.Errorf("re-encrypted, but could not drop the old keys from the key file: %w", err)
		}
	}
	retained := "the previous key is no longer accepted"
	if keepOld {
		retained = fmt.Sprintf("the previous key list stays readable (%d key(s)); drop it once every deployment has the new file", len(keys))
	}
	_, err = fmt.Fprintf(c.stderr, "re-encrypted %s with key %s; %s\nupdate FORGE_MASTER_KEY wherever it is stored (docs/CREDENTIALS.md, \"Rotating the key\")\n",
		filepath.Join(c.dir, credentials.FileName(env)), fresh.ID, retained)
	return err
}

func joinKeys(keys []encryption.Key) string {
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = encryption.EncodeKey(key.Material)
	}
	return strings.Join(parts, ",")
}
