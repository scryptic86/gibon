package main

import (
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

var (
	validPasteRegex = regexp.MustCompile(`/([a-z]|[A-Z]|[0-9]|\.)*$`)
	pasteMap        map[string][]byte
)

func getPaste(id string) ([]byte, bool) {
	p, ok := pasteMap[id]
	return p, ok
}

func addPaste(id string, p []byte) {
	pasteMap[id] = p
}

type pasteHandler struct{}

func (h *pasteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Check for valid paste ID
	pasteID := r.URL.EscapedPath()
	if !validPasteRegex.MatchString(pasteID) {
		http.Error(w, "Illegal paste name!", http.StatusServiceUnavailable)
		return
	}

	// Now should be in a valid paste ID format
	pasteID = strings.TrimPrefix(pasteID, "/")

	switch r.Method {
	case "GET":
		p, ok := getPaste(pasteID)
		if !ok {
			http.Error(w, "Paste not found!", http.StatusNotFound)
			return
		}
		w.Write(p)

	case "POST":
		_, ok := getPaste(pasteID)
		if ok {
			http.Error(w, "Paste with this ID already exists!", http.StatusBadRequest)
			return
		}

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed reading request body", http.StatusServiceUnavailable)
			return
		}
		defer r.Body.Close()

		addPaste(pasteID, body)
	}
}

func main() {
	bindAddr := flag.String("bind-addr", "localhost", "Bind to address")
	port := flag.Uint("port", 1024, "Bind to port")
	//cert := flag.String("cert", "", "TLS certificate file")
	//key := flag.String("key", "", "TLS key file")
	flag.Parse()

	pasteMap = make(map[string][]byte)
	pasteMap["test"] = []byte("test here!\n")

	err := http.ListenAndServe(*bindAddr+":"+strconv.Itoa(int(*port)) /**cert, *key,*/, &pasteHandler{})
	if err != nil {
		log.Fatalf(err.Error())
	}

	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	sig := <-signals
	log.Fatalf("Signal received %s...\n", sig)
}
