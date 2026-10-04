package hosts

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// SSHAliases reads the Host lines of the ssh config at path, following
// Include, and returns every name that is not a pattern, in file order
// and once each. A missing file has none.
func SSHAliases(path string) []string {
	var out []string
	seen := map[string]bool{}
	visited := map[string]bool{}
	base := filepath.Dir(path) // relative includes are under ~/.ssh
	var read func(path string, depth int)
	read = func(path string, depth int) {
		if depth > 8 || visited[path] {
			return
		}
		visited[path] = true
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			key, args := configLine(sc.Text())
			switch key {
			case "host":
				for _, a := range args {
					if strings.ContainsAny(a, "*?!") || seen[a] {
						continue
					}
					seen[a] = true
					out = append(out, a)
				}
			case "include":
				for _, a := range args {
					for _, p := range includePaths(base, a) {
						read(p, depth+1)
					}
				}
			}
		}
	}
	read(path, 0)
	return out
}

// configLine splits one ssh config line into its lower-cased keyword and
// its arguments ("Host a b", "Host=a", quoted words).
func configLine(line string) (string, []string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", nil
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), nil
	}
	key := strings.ToLower(line[:i])
	rest := strings.TrimLeft(line[i:], " \t")
	rest = strings.TrimLeft(strings.TrimPrefix(rest, "="), " \t")
	var args []string
	for rest != "" {
		var word string
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				word, rest = rest[1:], ""
			} else {
				word, rest = rest[1:end+1], rest[end+2:]
			}
		} else {
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				word, rest = rest, ""
			} else {
				word, rest = rest[:end], rest[end:]
			}
		}
		if word != "" {
			args = append(args, word)
		}
		rest = strings.TrimLeft(rest, " \t")
	}
	return key, args
}

// includePaths expands one Include argument: ~ is the home, a relative
// path is under base (the directory of the user's config), and globs
// expand.
func includePaths(base, arg string) []string {
	home, _ := os.UserHomeDir()
	switch {
	case strings.HasPrefix(arg, "~/"):
		arg = filepath.Join(home, arg[2:])
	case !filepath.IsAbs(arg):
		arg = filepath.Join(base, arg)
	}
	ms, err := filepath.Glob(arg)
	if err != nil {
		return nil
	}
	return ms
}
