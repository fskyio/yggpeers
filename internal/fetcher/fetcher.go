package fetcher

import (
	"bufio"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"yggpeers/internal/config"
	"yggpeers/internal/store"
)

const repoURL = "https://github.com/yggdrasil-network/public-peers.git"

var peerURIRe = regexp.MustCompile("`((?:tcp|tls|quic|ws|wss|socks|sockstls)://[^`\\r\\n]+)`")

type Fetcher struct {
	cfg   config.Config
	store *store.Store
}

func New(cfg config.Config, s *store.Store) *Fetcher {
	return &Fetcher{cfg: cfg, store: s}
}

func (f *Fetcher) FetchAndParse() error {
	if err := f.updateRepo(); err != nil {
		return err
	}
	return f.parseAndStore()
}

func (f *Fetcher) updateRepo() error {
	gitDir := filepath.Join(f.cfg.RepoDir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		log.Println("Updating public-peers repo...")
		cmd := exec.Command("git", "-C", f.cfg.RepoDir, "pull", "--ff-only")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	} else if !os.IsNotExist(err) {
		return err
	}

	// .git is missing — make sure the target directory itself is either
	// missing or empty before letting git clone touch it, so we don't
	// accidentally fight with whatever is already there.
	if entries, err := os.ReadDir(f.cfg.RepoDir); err == nil {
		if len(entries) > 0 {
			return fmt.Errorf("repo dir %q exists and is not empty but is not a git repo", f.cfg.RepoDir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(f.cfg.RepoDir), 0o755); err != nil {
		return err
	}
	log.Println("Cloning public-peers repo...")
	cmd := exec.Command("git", "clone", "--depth=1", repoURL, f.cfg.RepoDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (f *Fetcher) parseAndStore() error {
	var peers []store.PeerInput

	err := filepath.Walk(f.cfg.RepoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		if info.IsDir() || !strings.HasSuffix(base, ".md") || strings.EqualFold(base, "README.md") {
			return nil
		}

		parentDir := filepath.Base(filepath.Dir(path))
		isNetwork := parentDir == "other"
		country := filenameToCountry(base)
		uris, parseErr := parsePeerFile(path)
		if parseErr != nil {
			log.Printf("warn: parse %s: %v", path, parseErr)
			return nil
		}
		for _, uri := range uris {
			proto, host, port, err := parsePeerURI(uri)
			if err != nil {
				log.Printf("warn: parse URI %q: %v", uri, err)
				continue
			}
			if port == "" {
				continue
			}
			peers = append(peers, store.PeerInput{
				URI:       uri,
				Country:   country,
				Protocol:  proto,
				Host:      host,
				Port:      port,
				IsNetwork: isNetwork,
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		// Defensive: if parsing yielded nothing, don't wipe the existing
		// peer table. Likely a transient parse failure or empty checkout.
		log.Println("warn: parse produced no peers, skipping store update")
		return nil
	}
	return f.store.BulkUpsertPeers(peers)
}

func parsePeerFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var uris []string
	sc := bufio.NewScanner(f)
	// Allow long markdown lines (default is 64 KiB).
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		for _, m := range peerURIRe.FindAllStringSubmatch(sc.Text(), -1) {
			uris = append(uris, strings.TrimSpace(m[1]))
		}
	}
	return uris, sc.Err()
}

func parsePeerURI(raw string) (proto, host, port string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	return u.Scheme, u.Hostname(), u.Port(), nil
}

func filenameToCountry(filename string) string {
	name := strings.TrimSuffix(filename, ".md")
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}
