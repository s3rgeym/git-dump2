package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/time/rate"
)

type Config struct {
	InputPath string
	DumpDir   string
	UA        string
	Workers   int
	RPSLimit  int
	MaxErr    int
	MaxRetry  int
	Force     bool
	CT        time.Duration
	RT        time.Duration
}

type Job struct {
	BaseURL string
	Host    string
	Path    string
}

type Result struct {
	URL           string
	ContentType   string
	ContentLength int64
}

type hostTracker struct {
	count int64
	once  sync.Once
}

type Dumper struct {
	cfg          Config
	client       *http.Client
	limiter      *rate.Limiter
	seen         sync.Map
	errCounts    sync.Map
	blocked      sync.Map
	inflight     sync.WaitGroup
	jobs         chan Job
	results      chan Result
	hostTrackers sync.Map
}

var version = "dev"

var (
	cErr  = color.New(color.FgRed)
	cWarn = color.New(color.FgYellow)
	cSucc = color.New(color.FgGreen)
)

var (
	hashRegex = regexp.MustCompile(`^[0-9a-f]{40}$`)
	refsRegex = regexp.MustCompile(`refs/[^\s\^]+`)
	htmlRegex = regexp.MustCompile(`(?i)^\s*<(!DOCTYPE|html)`)
)

func printError(f string, a ...any)   { cErr.Fprintf(os.Stdout, "[-] "+f+"\n", a...) }
func printWarning(f string, a ...any) { cWarn.Fprintf(os.Stdout, "[!] "+f+"\n", a...) }
func printSuccess(f string, a ...any) { cSucc.Fprintf(os.Stdout, "[+] "+f+"\n", a...) }

func main() {
	var cfg Config
	flag.StringVar(&cfg.InputPath, "i", "-", "Input URLs")
	flag.StringVar(&cfg.DumpDir, "o", "dumps", "Output directory")
	flag.StringVar(&cfg.UA, "ua", "", "User-Agent")
	flag.IntVar(&cfg.Workers, "w", 10, "Workers")
	flag.IntVar(&cfg.RPSLimit, "rps", 50, "RPS")
	flag.IntVar(&cfg.MaxErr, "maxerr", 30, "Max errors per host")
	flag.IntVar(&cfg.MaxRetry, "maxretry", 1, "Max retries on connection error")
	flag.BoolVar(&cfg.Force, "f", false, "Force overwrite")
	flag.DurationVar(&cfg.CT, "ct", 5*time.Second, "Connect timeout")
	flag.DurationVar(&cfg.RT, "rt", 15*time.Second, "Request timeout")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &http.Client{
		Timeout: cfg.RT,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			DialContext:         (&net.Dialer{Timeout: cfg.CT}).DialContext,
			TLSHandshakeTimeout: cfg.CT,
		},
	}
	limiter := rate.NewLimiter(rate.Every(time.Second/time.Duration(cfg.RPSLimit)), 1)

	var in io.Reader = os.Stdin
	if cfg.InputPath != "-" && cfg.InputPath != "" {
		f, err := os.Open(cfg.InputPath)
		if err != nil {
			printError("input error: %v", err)
			return
		}
		defer f.Close()
		in = f
	}

	var urls []string
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			urls = append(urls, line)
		}
	}

	NewDumper(cfg, client, limiter).Run(ctx, urls)

	cleanup(cfg.DumpDir)
}

func NewDumper(cfg Config, client *http.Client, limiter *rate.Limiter) *Dumper {
	return &Dumper{
		cfg:     cfg,
		client:  client,
		limiter: limiter,
		jobs:    make(chan Job, cfg.Workers),
		results: make(chan Result, cfg.Workers),
	}
}

func (c *Dumper) getTracker(host string) *hostTracker {
	v, _ := c.hostTrackers.LoadOrStore(host, &hostTracker{})
	return v.(*hostTracker)
}

func (c *Dumper) addHost(host string) {
	atomic.AddInt64(&c.getTracker(host).count, 1)
}

func (c *Dumper) doneHost(host string) {
	t := c.getTracker(host)
	if atomic.AddInt64(&t.count, -1) == 0 {
		t.once.Do(func() {
			c.reconstructRepo(host)
		})
	}
}

func (c *Dumper) reconstructRepo(host string) {
	repoPath := filepath.Join(c.cfg.DumpDir, host)
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		return
	}
	printSuccess("Reconstructing %s", host)
	exec.Command("git", "-C", repoPath, "checkout", ".").Run()
	exec.Command("git", "-C", repoPath, "reset", "--hard").Run()
}

func (c *Dumper) Run(ctx context.Context, urls []string) {
	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(ctx)
		}()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for r := range c.results {
			printSuccess("%s", r.URL)
		}
	}()

	for _, rawURL := range urls {
		u, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		c.addHost(u.Host)
		c.inflight.Add(1)
		select {
		case c.jobs <- Job{BaseURL: rawURL, Host: u.Host, Path: "/.git/index"}:
		case <-ctx.Done():
			c.doneHost(u.Host)
			c.inflight.Done()
		}
	}

	c.inflight.Wait()
	close(c.jobs)
	wg.Wait()
	close(c.results)
	<-done
}

func (c *Dumper) worker(ctx context.Context) {
	for job := range c.jobs {
		if c.isBlocked(job.Host) {
			c.doneHost(job.Host)
			c.inflight.Done()
			continue
		}
		if err := c.limiter.Wait(ctx); err != nil {
			c.doneHost(job.Host)
			c.inflight.Done()
			return
		}
		c.process(ctx, job)
	}
}

func (c *Dumper) process(ctx context.Context, job Job) {
	defer c.inflight.Done()
	defer c.doneHost(job.Host)

	u, _ := url.Parse(job.BaseURL)
	u.Path = path.Join(u.Path, job.Path)
	target := u.String()
	localPath := filepath.Join(c.cfg.DumpDir, job.Host, u.Path)

	if !c.cfg.Force {
		if _, err := os.Stat(localPath); err == nil {
			return
		}
	}

	resp, err := c.fetch(ctx, target)
	if err != nil {
		printError("Error: %s -> %v", target, err)
		c.incErr(job.Host)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
	case 403:
		printError("403: %s", target)
		c.incErr(job.Host)
		return
	case 404:
		printError("404: %s", target)
		return
	default:
		printWarning("%d: %s", resp.StatusCode, target)
		return
	}

	peek := make([]byte, 512)
	n, _ := io.ReadAtLeast(resp.Body, peek, 16)
	dataHead := peek[:n]
	if htmlRegex.Match(dataHead) {
		printWarning("Skip HTML: %s", target)
		return
	}

	remaining, _ := io.ReadAll(resp.Body)
	data := append(dataHead, remaining...)
	contentType := resp.Header.Get("Content-Type")

	save(localPath, data)

	c.addHost(job.Host)
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		defer c.doneHost(job.Host)
		select {
		case c.results <- Result{
			URL:           target,
			ContentType:   contentType,
			ContentLength: int64(len(data)),
		}:
		case <-ctx.Done():
			return
		}
		c.parseContent(ctx, data, job.Path, job.BaseURL, job.Host)
	}()
}

// fetch выполняет GET с повторами при ошибках соединения.
func (c *Dumper) fetch(ctx context.Context, target string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetry; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		req.Header.Set("User-Agent", getUA(c.cfg.UA))
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func (c *Dumper) parseContent(ctx context.Context, raw []byte, gitPath, baseURL, host string) {
	switch {
	case strings.HasSuffix(gitPath, "index"):
		idx := &index.Index{}
		if err := index.NewDecoder(bytes.NewReader(raw)).Decode(idx); err != nil {
			return
		}
		for _, ep := range []string{
			"/.git/HEAD", "/.git/config", "/.git/packed-refs", "/.git/logs/HEAD",
			"/.git/refs/heads/master", "/.git/refs/heads/main",
			"/.git/objects/info/packs",
		} {
			c.enqueuePath(ctx, baseURL, host, ep)
		}
		for _, e := range idx.Entries {
			c.enqueueHash(ctx, baseURL, host, e.Hash.String())
		}

	case strings.HasSuffix(gitPath, "objects/info/packs"):
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "P ") {
				continue
			}
			packName := strings.TrimSpace(line[2:])
			c.enqueuePath(ctx, baseURL, host, "/.git/objects/pack/"+packName)
			c.enqueuePath(ctx, baseURL, host, "/.git/objects/pack/"+strings.Replace(packName, ".pack", ".idx", 1))
		}

	case strings.Contains(gitPath, "/.git/objects/") && !strings.Contains(gitPath, "/pack/"):
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return
		}
		defer zr.Close()
		decoded, _ := io.ReadAll(zr)

		obj := &plumbing.MemoryObject{}
		obj.SetSize(int64(len(decoded)))
		w, _ := obj.Writer()
		w.Write(decoded)
		w.Close()

		func() {
			defer func() { recover() }()
			parsed, err := object.DecodeObject(nil, obj)
			if err != nil {
				return
			}
			switch o := parsed.(type) {
			case *object.Commit:
				c.enqueueHash(ctx, baseURL, host, o.TreeHash.String())
				for _, ph := range o.ParentHashes {
					c.enqueueHash(ctx, baseURL, host, ph.String())
				}
			case *object.Tree:
				for _, te := range o.Entries {
					c.enqueueHash(ctx, baseURL, host, te.Hash.String())
				}
			}
		}()

	case strings.Contains(gitPath, "/.git/"):
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		for scanner.Scan() {
			line := scanner.Text()
			for _, part := range strings.Fields(line) {
				if hashRegex.MatchString(part) {
					c.enqueueHash(ctx, baseURL, host, part)
				}
			}
			if strings.Contains(line, "refs/") {
				if ref := refsRegex.FindString(line); ref != "" {
					c.enqueuePath(ctx, baseURL, host, "/.git/"+ref)
				}
			}
		}
	}
}

func (c *Dumper) enqueueHash(ctx context.Context, baseURL, host, hash string) {
	key := baseURL + hash
	if _, loaded := c.seen.LoadOrStore(key, struct{}{}); !loaded {
		c.addHost(host)
		c.inflight.Add(1)
		select {
		case c.jobs <- Job{BaseURL: baseURL, Host: host, Path: fmt.Sprintf("/.git/objects/%s/%s", hash[:2], hash[2:])}:
		case <-ctx.Done():
			c.doneHost(host)
			c.inflight.Done()
		}
	}
}

func (c *Dumper) enqueuePath(ctx context.Context, baseURL, host, gitPath string) {
	key := baseURL + gitPath
	if _, loaded := c.seen.LoadOrStore(key, struct{}{}); !loaded {
		c.addHost(host)
		c.inflight.Add(1)
		select {
		case c.jobs <- Job{BaseURL: baseURL, Host: host, Path: gitPath}:
		case <-ctx.Done():
			c.doneHost(host)
			c.inflight.Done()
		}
	}
}

func (c *Dumper) incErr(host string) {
	val, _ := c.errCounts.LoadOrStore(host, new(int32))
	n := atomic.AddInt32(val.(*int32), 1)
	if int(n) >= c.cfg.MaxErr {
		if _, loaded := c.blocked.LoadOrStore(host, true); !loaded {
			printError("%s blocked after %d errors", host, n)
		}
	}
}

func (c *Dumper) isBlocked(host string) bool {
	_, ok := c.blocked.Load(host)
	return ok
}

func save(localPath string, data []byte) {
	os.MkdirAll(filepath.Dir(localPath), 0755)
	os.WriteFile(localPath, data, 0644)
}

func getUA(ua string) string {
	if ua != "" {
		return ua
	}
	platforms := []string{"Windows NT 10.0; Win64; x64", "Macintosh; Intel Mac OS X 10_15_7", "X11; Linux x86_64"}
	p := platforms[rand.Intn(len(platforms))]
	v := fmt.Sprintf("13%d.0.0.0", rand.Intn(7))
	return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36", p, v)
}

func cleanup(root string) {
	var dirs []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, _ := os.ReadDir(dirs[i])
		if len(entries) == 0 {
			os.Remove(dirs[i])
		}
	}
}
