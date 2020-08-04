package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/core/coreapi"
	"github.com/ipfs/go-ipfs/core/node/libp2p"
	"github.com/ipfs/go-ipfs/plugin/loader"
	"github.com/ipfs/go-ipfs/repo/fsrepo"

	files "github.com/ipfs/go-ipfs-files"
	icore "github.com/ipfs/interface-go-ipfs-core"
	icorepath "github.com/ipfs/interface-go-ipfs-core/path"
)

const (
	pastePrefix = "/paste/"
	ipfsPrefix  = "/ipfs/"

	maxPasteSize = 1048576

	unixfsGetTimeout = time.Millisecond * 250

	rootHelpStr = `Gibon -- an IPFS-backed pastebin service!

Usage:
POST example.com 'paste text goes here'
--> '/paste/<PASTE_ID>'

GET example.com/paste/<PASTE_ID>
--> 'paste text goes here'

e.g.
$ curl example.com --data "$(cat somefile.txt)"
/paste/Qmenmh8JwVoUGc4tCj3us9bx8YmADCHGEkcHjUUVsxByVN

$ curl example.com/paste/Qmenmh8JwVoUGc4tCj3us9bx8YmADCHGEkcHjUUVsxByVN
<output of somefile.txt>
`
)

type pasteHandler struct {
	ctx  context.Context
	ipfs icore.CoreAPI
}

func (h *pasteHandler) getPaste(pathStr string) (io.Reader, error) {
	// Create new IPFS path from input
	ipfsPath := icorepath.New(pathStr)

	// Get new deadline context (timeout on no paste found)
	ctx, cancel := context.WithDeadline(h.ctx, time.Now().Add(unixfsGetTimeout))
	defer cancel()

	// Attempt to retrieve node for path
	node, err := h.ipfs.Unixfs().Get(ctx, ipfsPath)
	if err != nil {
		return nil, err
	}

	// Check if node is a file, if so then cast it!
	file, ok := node.(files.File)
	if !ok {
		return nil, errors.New("IPFS node not file")
	}

	// Read up to maxPasteSize of block contents
	return io.LimitReader(file, maxPasteSize), nil
}

func (h *pasteHandler) putPaste(body io.ReadCloser) (string, error) {
	// Wrap body in File object, defer close
	file := files.NewReaderFile(body)
	defer body.Close()

	// Add new file object to the IPFS store via the Unixfs API
	rPath, err := h.ipfs.Unixfs().Add(h.ctx, file)
	if err != nil {
		return "", err
	}

	// Return the resolved path
	return rPath.String(), nil
}

func (h *pasteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Check for valid paste ID
	rawPath := r.URL.EscapedPath()
	log.Printf("Serve: %s %s\n", r.Method, rawPath)

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
		reader, err := h.getPaste(rawPath)
		if err != nil {
			log.Printf("Paste not retrieved - %s\n", err.Error())
			http.Error(w, "Paste not found!", http.StatusNotFound)
			return
		}

		// Write the paste!
		w.Header().Set("content-type", "text/plain")
		io.Copy(w, reader)

	case "POST":
		// If not at root, send error
		if rawPath != "/" {
			log.Println("Paste POST request to non-root path")
			http.Error(w, "Please POST new pastes to site root!", http.StatusBadRequest)
			return
		}

		// Set max read size to 1MB
		r.Body = http.MaxBytesReader(w, r.Body, maxPasteSize)

		// Place the byte slice in the IPFS store
		pathStr, err := h.putPaste(r.Body)
		if err != nil {
			log.Printf("Failed to put paste in store - %s\n", err.Error())
			http.Error(w, "Failed to put paste at CID", http.StatusInternalServerError)
			return
		}
		pathStr = strings.Replace(pathStr, ipfsPrefix, pastePrefix, 1)

		// Write the store path in response
		w.Header().Set("content-type", "text/plain")
		w.Write([]byte(pathStr))
	}
}

func constructIPFSNodeAPI(ctx context.Context, repoPath string) (icore.CoreAPI, error) {
	// Load any external plugins
	plugins, err := loader.NewPluginLoader(path.Join(repoPath, "plugins"))
	if err != nil {
		return nil, err
	}
	log.Println("Loaded external IPFS repo plugins")

	// Load preloaded and external plugins
	err = plugins.Initialize()
	if err != nil {
		return nil, err
	}
	log.Println("... initialized!")

	// Inject the plugins
	err = plugins.Inject()
	if err != nil {
		return nil, err
	}
	log.Println("... injected!")

	// Open the repo
	repo, err := fsrepo.Open(repoPath)
	if err != nil {
		return nil, err
	}
	log.Println("IPFS repo path opened")

	// Construct the node
	node, err := core.NewNode(
		ctx,
		&core.BuildCfg{
			Online:  true,
			Routing: libp2p.DHTOption,
			Repo:    repo,
		},
	)
	if err != nil {
		return nil, err
	}
	log.Println("IPFS node constructed!")

	// Return core API wrapping the node
	return coreapi.NewCoreAPI(node)
}

func main() {
	httpBindAddr := flag.String("http-bind-addr", "localhost", "Bind HTTP server to address")
	httpPort := flag.Uint("http-port", 443, "Bind HTTP server to port")
	ipfsRepo := flag.String("ipfs-repo", "/var/ipfs", "IPFS repo path")
	certFile := flag.String("cert-file", "", "TLS certificate file")
	keyFile := flag.String("key-file", "", "TLS key file")
	flag.Parse()

	// Check we have been supplied IPFS repo
	if *ipfsRepo == "" {
		log.Fatalf("No IPFS repo path supplied!")
	}

	// Check we have been supplied necessary TLS cert + Key files
	if *certFile == "" {
		log.Fatalf("No TLS certificate file supplied!")
	} else if *keyFile == "" {
		log.Fatalf("No TLS key file supplied!")
	}

	// Get current context (cancellable)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Get new IPFS node API instance
	coreAPI, err := constructIPFSNodeAPI(ctx, *ipfsRepo)
	if err != nil {
		log.Fatalf(err.Error())
	}

	// Create new HTTP server object
	httpAddr := *httpBindAddr + ":" + strconv.Itoa(int(*httpPort))
	server := &http.Server{
		Addr:              httpAddr,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       2 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           &pasteHandler{ctx, coreAPI},
	}

	// Start HTTP server!
	go func() {
		err = server.ListenAndServeTLS(*certFile, *keyFile)
		if err != nil {
			log.Fatalf(err.Error())
		}
	}()
	log.Println("HTTP server started!")

	// Setup channel for OS signals
	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	log.Println("Listening for OS signals...")

	// Exit on signal
	sig := <-signals
	log.Fatalf("Signal received %s, stopping!\n", sig)
}
