package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"syscall"

	"github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/core/coreapi"
	"github.com/ipfs/go-ipfs/core/node/libp2p"
	"github.com/ipfs/go-ipfs/plugin/loader"
	"github.com/ipfs/go-ipfs/repo/fsrepo"

	files "github.com/ipfs/go-ipfs-files"
	icore "github.com/ipfs/interface-go-ipfs-core"
	icorepath "github.com/ipfs/interface-go-ipfs-core/path"
)

type pasteHandler struct {
	ctx  context.Context
	ipfs icore.CoreAPI
}

func (h *pasteHandler) getPaste(cidStr string) ([]byte, error) {
	cidPath := icorepath.New(cidStr)
	r, err := h.ipfs.Block().Get(h.ctx, cidPath)
	if err != nil {
		return nil, err
	}
	return ioutil.ReadAll(r)
}

func (h *pasteHandler) putPaste(paste []byte) (string, error) {
	file := files.NewBytesFile(paste)
	stat, err := h.ipfs.Block().Put(h.ctx, file)
	if err != nil {
		return "", err
	}
	return stat.Path().String(), nil
}

func (h *pasteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Check for valid paste ID
	rawPath := r.URL.EscapedPath()

	switch r.Method {
	case "GET":
		// At root send help string
		if rawPath == "/" {
			w.Write([]byte("Gibon -- IPFS-backed pastebin service!\n"))
			return
		}

		// Try look for paste with CID
		b, err := h.getPaste(rawPath)
		if err != nil {
			log.Println(err.Error())
			http.Error(w, "Paste not found!", http.StatusNotFound)
			return
		}
		w.Write(b)

	case "POST":
		// If not at root, send error
		if rawPath != "/" {
			http.Error(w, "Please POST new pastes to site root!", http.StatusBadRequest)
			return
		}

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Println(err.Error())
			http.Error(w, "Failed reading request body", http.StatusServiceUnavailable)
			return
		}
		defer r.Body.Close()

		cidStr, err := h.putPaste(body)
		if err != nil {
			log.Println(err.Error())
			http.Error(w, "Failed to put paste at CID", http.StatusInternalServerError)
			return
		}

		w.Write([]byte(cidStr))
	}
}

func constructIPFSNodeAPI(ctx context.Context, repoPath string) (icore.CoreAPI, error) {
	// Load any external plugins
	plugins, err := loader.NewPluginLoader(path.Join(repoPath, "plugins"))
	if err != nil {
		return nil, err
	}

	// Load preloaded and external plugins
	err = plugins.Initialize()
	if err != nil {
		return nil, err
	}

	// Inject the plugins
	err = plugins.Inject()
	if err != nil {
		return nil, err
	}

	// Open the repo
	repo, err := fsrepo.Open(repoPath)
	if err != nil {
		return nil, err
	}

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

	// Return core API wrapping the node
	return coreapi.NewCoreAPI(node)
}

func main() {
	httpBindAddr := flag.String("http-bind-addr", "localhost", "Bind HTTP server to address")
	httpPort := flag.Uint("http-port", 443, "Bind HTTP server to port")
	ipfsRepo := flag.String("ipfs-repo", "/var/ipfs", "IPFS repo path")
	flag.Parse()

	// Get current context (cancellable)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Get new IPFS node API instance
	coreAPI, err := constructIPFSNodeAPI(ctx, *ipfsRepo)
	if err != nil {
		log.Fatalf(err.Error())
	}

	// Create new pasteHandler instance to handle HTTP side
	handler := &pasteHandler{
		ctx,
		coreAPI,
	}

	// Start HTTP server
	httpAddr := *httpBindAddr + ":" + strconv.Itoa(int(*httpPort))
	err = http.ListenAndServe(httpAddr /**cert, *key,*/, handler)
	if err != nil {
		log.Fatalf(err.Error())
	}

	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	sig := <-signals
	log.Fatalf("Signal received %s...\n", sig)
}
