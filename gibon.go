package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
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
	"github.com/julienschmidt/httprouter"

	config "github.com/ipfs/go-ipfs-config"
	icore "github.com/ipfs/interface-go-ipfs-core"
	icorepath "github.com/ipfs/interface-go-ipfs-core/path"
)

const (
	pastePrefix = "/paste/"
	ipfsPrefix  = "/ipld/"

	maxPasteSize = 1048576

	unixfsGetTimeout = time.Millisecond * 250
)

var (
	rootHelpStr = `Gibon -- an IPFS-backed pastebin service with encryption support!

Usage:
$ curl https://%s --data 'paste text goes here'
--> '/paste/<PASTE_ID>'

$ curl https://%s/paste/<PASTE_ID>
--> 'paste text goes here'

$ curl https://%s/?key=awful_password --data 'paste text goes here'
--> '/paste/<PASTE_ID>'

$ curl https://%s/paste/<PASTE_ID>?key=awful_password
--> 'paste text goes here'
`

	globalContext context.Context
	globalCancel  func()

	ipfsAPI icore.CoreAPI
)

type paste struct {
	text []byte
}

func (p *paste) encrypt(key string) error {
	// Get new GCM wrapped AES block cipher for key
	gcmBlockCipher, err := newAESGCMBlockCiperForKey(key)
	if err != nil {
		return err
	}

	// Create nonce of requested length
	nonce := make([]byte, gcmBlockCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}

	// Create cipher text
	cipherText := gcmBlockCipher.Seal(
		nil,
		nonce,
		p.text,
		nil,
	)

	// Set paste text as nonce+cipherText
	p.text = append(nonce, cipherText...)

	// Return all good :)
	return nil
}

func (p *paste) decrypt(key string) error {
	// Get new GCM wrapped AES block cipher for key
	gcmBlockCipher, err := newAESGCMBlockCiperForKey(key)
	if err != nil {
		return err
	}

	// Ensure paste long enough for nonce
	if gcmBlockCipher.NonceSize() > len(p.text) {
		return errors.New("text not long enough to contain nonce")
	}

	// Try decrypt using nonce and cipherText from raw paste text
	text, err := gcmBlockCipher.Open(
		nil,
		p.text[:gcmBlockCipher.NonceSize()],
		p.text[gcmBlockCipher.NonceSize():],
		nil,
	)
	if err != nil {
		return err
	}

	// Set new decrypted text, set not-encrypted
	p.text = text

	return nil
}

func newAESGCMBlockCiperForKey(key string) (cipher.AEAD, error) {
	// Hash the supplied key
	hash := sha256.Sum256([]byte(key))

	// Create new AES block cipher based on key
	blockCipher, err := aes.NewCipher(hash[:])
	if err != nil {
		return nil, err
	}

	// Return block cipher wrapped in GCM
	return cipher.NewGCM(blockCipher)
}

type pasteHandler struct {
	ipfs icore.CoreAPI
}

func getPaste(pathStr string) (*paste, error) {
	// Create new IPFS path from input
	ipfsPath := icorepath.New(pathStr)

	// Get new deadline context (timeout on no paste found)
	ctx, cancel := context.WithDeadline(globalContext, time.Now().Add(unixfsGetTimeout))
	defer cancel()

	// Get reader for object
	reader, err := ipfsAPI.Block().Get(ctx, ipfsPath)
	if err != nil {
		return nil, err
	}

	// Read from the supplied reader
	b, err := ioutil.ReadAll(io.LimitReader(reader, maxPasteSize))
	if err != nil {
		return nil, err
	}

	// Return the paste
	return &paste{b}, nil
}

func putPaste(p *paste) (string, error) {
	// Create new bytes reader based on Paste JSON
	reader := bytes.NewReader(p.text)

	// Put Paste JSON in IPFS storage
	stat, err := ipfsAPI.Block().Put(globalContext, reader)
	if err != nil {
		return "", err
	}

	// Return the resolved path
	return stat.Path().String(), nil
}

func helpHandler(writer http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	writer.Header().Set("content-type", "text/plain")
	writer.Write([]byte(rootHelpStr))
}

func getPasteHandler(writer http.ResponseWriter, request *http.Request, params httprouter.Params) {
	// Get paste path
	pastePath := ipfsPrefix + params.ByName("cid")

	// Try look for paste with CID
	p, err := getPaste(pastePath)
	if err != nil {
		log.Printf("Paste not retrieved - %s\n", err.Error())
		http.Error(writer, "Paste not found!", http.StatusNotFound)
		return
	}

	// If decryption key supplied, try decrypt
	if key := request.URL.Query().Get("key"); key != "" {
		err = p.decrypt(key)
		if err != nil {
			log.Printf("Failed to decrypt paste - %s\n", err.Error())
			http.Error(writer, "Paste decryption failed!", http.StatusInternalServerError)
			return
		}
	}

	// Write the paste!
	writer.Header().Set("content-type", "text/plain")
	writer.Write(p.text)
}

func putPasteHandler(writer http.ResponseWriter, request *http.Request, _ httprouter.Params) {
	// Set max read size to 1MB
	request.Body = http.MaxBytesReader(writer, request.Body, maxPasteSize)

	// Read body content
	b, err := ioutil.ReadAll(request.Body)
	if err != nil {
		log.Println("Failed to read request body")
		http.Error(writer, "Failed to read request", http.StatusInternalServerError)
		return
	}

	// Create new paste, if encryption key provided, try encrypt!
	p := &paste{b}
	if key := request.URL.Query().Get("key"); key != "" {
		err = p.encrypt(key)
		if err != nil {
			log.Printf("Failed to encrypt paste - %s\n", err.Error())
			http.Error(writer, "Paste encryption failed!", http.StatusInternalServerError)
			return
		}
	}

	// Place the paste into the IPFS store
	pathStr, err := putPaste(p)
	if err != nil {
		log.Printf("Failed to put paste in store - %s\n", err.Error())
		http.Error(writer, "Failed to put paste in store", http.StatusInternalServerError)
		return
	}
	pathStr = strings.Replace(pathStr, ipfsPrefix, pastePrefix, 1)

	// Write the store path in response
	writer.Header().Set("content-type", "text/plain")
	writer.Write([]byte(pathStr))
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
			Online:  false,
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

func init() {
	// As part of init perform initial entropy assertion
	b := make([]byte, 1)
	_, err := io.ReadFull(rand.Reader, b)
	if err != nil {
		fatalf("Failed to assert safe source of system entropy exists!")
	}
}

func main() {
	// Define error here
	var err error

	// Set flags and parse!
	httpHostname := flag.String("http-hostname", "", "Set HTTP hostname for printed help message")
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
		err = setupIPFSPlugins("")
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
		err = setupIPFSPlugins(*ipfsRepo)
		if err != nil {
			fatalf(err.Error())
		}
	}

	// Get new IPFS node API instance
	ipfsAPI, err = constructIPFSNodeAPI(*ipfsRepo)
	if err != nil {
		fatalf(err.Error())
	}

	// Setup HTTP router
	router := &httprouter.Router{
		RedirectTrailingSlash:  true,
		RedirectFixedPath:      true,
		HandleMethodNotAllowed: true,
		HandleOPTIONS:          false,
		PanicHandler: func(writer http.ResponseWriter, _ *http.Request, _ interface{}) {
			http.Error(writer, "Unknown error occurred!", http.StatusServiceUnavailable)
		},
	}

	// Add HTTP routes
	router.GET("/", helpHandler)
	router.POST("/", putPasteHandler)
	router.GET(pastePrefix+":cid", getPasteHandler)

	// Create new HTTP server object
	httpAddr := *httpBindAddr + ":" + strconv.Itoa(int(*httpPort))
	server := &http.Server{
		Addr:              httpAddr,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       2 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           router,
	}

	// If hostname not set, use httpAddr
	if *httpHostname == "" {
		*httpHostname = httpAddr
	}

	// Construct the HTTP root site help string
	rootHelpStr = fmt.Sprintf(rootHelpStr, *httpHostname, *httpHostname, *httpHostname, *httpHostname)

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
