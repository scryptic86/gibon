package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/core/coreapi"
	"github.com/ipfs/go-ipfs/core/node/libp2p"
	"github.com/ipfs/go-ipfs/plugin/loader"
	"github.com/ipfs/go-ipfs/repo/fsrepo"

	config "github.com/ipfs/go-ipfs-config"
	icore "github.com/ipfs/interface-go-ipfs-core"
	icorepath "github.com/ipfs/interface-go-ipfs-core/path"
)

const (
	pastePrefix = "/paste/"
	ipfsPrefix  = "/ipld/"

	maxPasteSize = 1048576
	maxTitleSize = 100
	maxJSONSize  = maxPasteSize + maxTitleSize + 100

	unixfsGetTimeout = time.Millisecond * 250

	rootHelpStr = `Gibon -- an IPFS-backed pastebin service!

Usage:
POST example.com 'paste text goes here'
--> '/paste/<PASTE_ID>'

GET example.com/paste/<PASTE_ID>
--> 'paste text goes here'

e.g.
$ curl example.com/filename.sh --data "$(cat somefile.txt)"
/paste/Qmenmh8JwVoUGc4tCj3us9bx8YmADCHGEkcHjUUVsxByVN

$ curl example.com/paste/Qmenmh8JwVoUGc4tCj3us9bx8YmADCHGEkcHjUUVsxByVN
<output of somefile.txt>

$ curl example.com/paste/Qmenmh8JwVoUGc4tCj3us9bx8YmADCHGEkcHjUUVsxByVN --header 'content-type: application/json'
{"name":"filename.sh","text":"<output of somefile.txt>"}
`
)

var (
	globalContext context.Context
	globalCancel  func()

	validPasteName = regexp.MustCompile(`^([a-z]|[A-Z]|[0-9]|\.)*$`)
)

type Paste struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

func newPaste(name string, raw []byte) *Paste {
	return &Paste{
		Name: name,
		Text: string(raw),
	}
}

func pasteFromJSON(b []byte) (*Paste, error) {
	p := &Paste{}
	err := json.Unmarshal(b, p)
	return p, err
}

func (p *Paste) toJSON() []byte {
	return []byte(`{"name":"` + p.Name + `","text":"` + p.Text + `"}`)
}

type pasteHandler struct {
	ipfs icore.CoreAPI
}

func (h *pasteHandler) getPaste(pathStr string) (*Paste, error) {
	// Create new IPFS path from input
	ipfsPath := icorepath.New(pathStr)

	// Get new deadline context (timeout on no paste found)
	ctx, cancel := context.WithDeadline(globalContext, time.Now().Add(unixfsGetTimeout))
	defer cancel()

	// Get reader for object
	reader, err := h.ipfs.Block().Get(ctx, ipfsPath)
	if err != nil {
		return nil, err
	}

	// Read from the supplied reader
	b, err := ioutil.ReadAll(io.LimitReader(reader, maxJSONSize))
	if err != nil {
		return nil, err
	}

	// Return the paste
	return pasteFromJSON(b)
}

func (h *pasteHandler) putPaste(p *Paste) (string, error) {
	// Create new bytes reader based on Paste JSON
	reader := bytes.NewReader(p.toJSON())

	// Put Paste JSON in IPFS storage
	stat, err := h.ipfs.Block().Put(globalContext, reader)
	if err != nil {
		return "", err
	}

	// Return the resolved path
	return stat.Path().String(), nil
}

func (h *pasteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Get escaped path
	rawPath := r.URL.EscapedPath()

	// Ensure is absolute and within size bounds
	if !path.IsAbs(rawPath) || len(rawPath) > maxTitleSize {
		log.Printf("Illegal paste path requested: %s\n", rawPath)
		http.Error(w, "Illegal paste path!", http.StatusNotAcceptable)
		return
	}

	// Log request
	log.Printf("(%s) %s %s\n", r.RemoteAddr, r.Method, rawPath)

	switch r.Method {
	case "GET":
		// At root send help string
		if rawPath == "/" {
			w.Write([]byte(rootHelpStr))
			return
		}

		// Ensure has paste prefix then correct to IPFS path
		if !strings.HasPrefix(rawPath, pastePrefix) {
			log.Printf("Illegal paste path requested: %s\n", rawPath)
			http.Error(w, "Illegal paste path!", http.StatusNotAcceptable)
			return
		}
		rawPath = strings.Replace(rawPath, pastePrefix, ipfsPrefix, 1)

		// Try look for paste with CID
		p, err := h.getPaste(rawPath)
		if err != nil {
			log.Printf("Paste not retrieved - %s\n", err.Error())
			http.Error(w, "Paste not found!", http.StatusNotFound)
			return
		}

		// Write the paste!
		if r.Header.Get("content-type") == "application/json" {
			w.Header().Set("content-type", "application/json")
			w.Write([]byte(p.toJSON()))
		} else {
			w.Header().Set("content-type", "text/plain")
			w.Write([]byte(p.Text))
		}

	case "POST":
		// Get raw path without leading /
		rawPath = strings.TrimPrefix(rawPath, "/")

		// Check for valid paste name
		if !validPasteName.MatchString(rawPath) {
			log.Printf("Invalid paste name: %s\n", rawPath)
			http.Error(w, "Invalid paste name!", http.StatusBadRequest)
			return
		}

		// Set max read size to 1MB
		r.Body = http.MaxBytesReader(w, r.Body, maxPasteSize)

		// Read body content
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Println("Failed to read request body")
			http.Error(w, "Failed to read request", http.StatusInternalServerError)
			return
		}

		// Place the paste into the IPFS store
		pathStr, err := h.putPaste(newPaste(rawPath, b))
		if err != nil {
			log.Printf("Failed to put paste in store - %s\n", err.Error())
			http.Error(w, "Failed to put paste in store", http.StatusInternalServerError)
			return
		}
		pathStr = strings.Replace(pathStr, ipfsPrefix, pastePrefix, 1)

		// Write the store path in response
		w.Header().Set("content-type", "text/plain")
		w.Write([]byte(pathStr))
	}
}

func initIPFSRepo(repoPath string) error {
	// Check repo path actually exists (and accessible)
	_, err := os.Stat(repoPath)
	if err != nil {
		return err
	}

	// Directory exists, check we can write
	testPath := path.Join(repoPath, "test")
	fd, err := os.Create(testPath)
	if err != nil {
		if os.IsPermission(err) {
			return errors.New("Repo path is not writable")
		}
		return err
	}

	// Close and delete test file
	fd.Close()
	os.Remove(testPath)

	// Init new repo config
	log.Println("Generating new IPFS config...")
	cfg, err := config.Init(log.Writer(), 4096)
	if err != nil {
		return err
	}

	// Init new repo on repo path
	log.Println("Initializing new IPFS repo...")
	err = fsrepo.Init(repoPath, cfg)
	if err != nil {
		return err
	}

	return nil
}

func setupIPFSPlugins(repoPath string) error {
	// Load any external plugins
	log.Println("Loading external IPFS repo plugins")
	plugins, err := loader.NewPluginLoader(path.Join(repoPath, "plugins"))
	if err != nil {
		return err
	}

	// Load preloaded and external plugins
	log.Println("... initializing...")
	err = plugins.Initialize()
	if err != nil {
		return err
	}

	// Inject the plugins
	log.Println("... injecting...")
	err = plugins.Inject()
	if err != nil {
		return err
	}

	return nil
}

func constructIPFSNodeAPI(repoPath string) (icore.CoreAPI, error) {
	// Open the repo
	log.Println("Opening IPFS repo path...")
	repo, err := fsrepo.Open(repoPath)
	if err != nil {
		return nil, err
	}

	// Construct the node
	log.Println("Constructing IPFS node object...")
	node, err := core.NewNode(
		globalContext,
		&core.BuildCfg{
			Online:  true,
			Routing: libp2p.DHTOption,
			Repo:    repo,
		},
	)
	if err != nil {
		return nil, err
	}

	// Return core API wrapping the node
	log.Println("Wrapping IPFS node in core API...")
	return coreapi.NewCoreAPI(node)
}

func fatalf(fmt string, args ...interface{}) {
	// Cancel global context if non-nil
	if globalCancel != nil {
		globalCancel()
	}

	// Finally, log fatal
	log.Fatalf(fmt, args...)
}

func main() {
	httpBindAddr := flag.String("http-bind-addr", "localhost", "Bind HTTP server to address")
	httpPort := flag.Uint("http-port", 443, "Bind HTTP server to port")
	ipfsRepo := flag.String("ipfs-repo", "", "IPFS repo path")
	certFile := flag.String("cert-file", "", "TLS certificate file")
	keyFile := flag.String("key-file", "", "TLS key file")
	flag.Parse()

	// Get current context (cancellable)
	globalContext, globalCancel = context.WithCancel(context.Background())

	// Check we have been supplied IPFS repo
	if *ipfsRepo == "" {
		fatalf("No IPFS repo path supplied!")
	}

	// Check we have been supplied necessary TLS cert + Key files
	if *certFile == "" {
		fatalf("No TLS certificate file supplied!")
	} else if *keyFile == "" {
		fatalf("No TLS key file supplied!")
	}

	// Check if repo initialized
	if !fsrepo.IsInitialized(*ipfsRepo) {
		log.Printf("IPFS repo at %s does not exist!\n", *ipfsRepo)

		// First load plugins
		err := setupIPFSPlugins("")
		if err != nil {
			fatalf(err.Error())
		}

		// Try initialize repo
		err = initIPFSRepo(*ipfsRepo)
		if err != nil {
			fatalf(err.Error())
		}
	} else {
		// First load plugins
		err := setupIPFSPlugins(*ipfsRepo)
		if err != nil {
			fatalf(err.Error())
		}
	}

	// Get new IPFS node API instance
	coreAPI, err := constructIPFSNodeAPI(*ipfsRepo)
	if err != nil {
		fatalf(err.Error())
	}

	// Create new HTTP server object
	httpAddr := *httpBindAddr + ":" + strconv.Itoa(int(*httpPort))
	server := &http.Server{
		Addr:              httpAddr,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       2 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           &pasteHandler{coreAPI},
	}

	// Start HTTP server!
	log.Printf("Starting HTTP server on: %s\n", httpAddr)
	go func() {
		err = server.ListenAndServeTLS(*certFile, *keyFile)
		if err != nil {
			fatalf(err.Error())
		}
	}()

	// Setup channel for OS signals
	log.Println("Listening for OS signals...")
	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	// Exit on signal
	sig := <-signals
	fatalf("Signal received %s, stopping!\n", sig)
}
