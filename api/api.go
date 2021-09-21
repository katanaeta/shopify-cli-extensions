// The API package implements an HTTP interface that is responsible for
// - serving build artifacts
// - sending build status updates via websocket
// - provide metadata in form of a manifest to the UI Extension host on the client
//
package api

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sync"
	"text/template"
	"time"

	"github.com/Shopify/shopify-cli-extensions/core"
	"github.com/Shopify/shopify-cli-extensions/create/fsutils"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

//go:embed templates/*
var templates embed.FS

func New(config *core.Config) *ExtensionsApi {
	mux := mux.NewRouter().StrictSlash(true)
	fs := fsutils.NewFS(&templates, "templates")

	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "/extensions/", http.StatusTemporaryRedirect)
	})

	api := configureExtensionsApi(config, mux, fs)

	return api
}

func (api *ExtensionsApi) Notify(statusUpdate StatusUpdate) {
	api.connections.Range(func(_, clientHandlers interface{}) bool {
		clientHandlers.(client).notify(statusUpdate)
		return true
	})
}

func (api *ExtensionsApi) Shutdown() {
	api.connections.Range(func(_, clientHandlers interface{}) bool {
		clientHandlers.(client).close(1000, "server shut down")
		return true
	})
}

func configureExtensionsApi(config *core.Config, router *mux.Router, fs *fsutils.FS) *ExtensionsApi {
	api := &ExtensionsApi{
		core.NewExtensionService(config),
		router,
		sync.Map{},
		fs,
	}

	api.HandleFunc("/extensions", api.extensionsHandler)

	for _, extension := range api.Extensions {
		assets := fmt.Sprintf("/extensions/%s/assets/", extension.UUID)
		buildDir := filepath.Join(".", extension.Development.RootDir, extension.Development.BuildDir)
		api.PathPrefix(assets).Handler(
			http.StripPrefix(assets, http.FileServer(http.Dir(buildDir))),
		)
	}

	api.HandleFunc("/extensions/{uuid:(?:[a-z]|[0-9]|-)+}", api.extensionRoot)

	return api
}

func (api *ExtensionsApi) extensionsHandler(rw http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		api.sendStatusUpdates(rw, r)
	} else {
		api.listExtensions(rw, r)
	}
}

func (api *ExtensionsApi) sendStatusUpdates(rw http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	connection, err := upgrader.Upgrade(rw, r, nil)
	if err != nil {
		return
	}

	notifications := make(chan StatusUpdate)

	close := func(closeCode int, message string) error {
		api.unregisterClient(connection, closeCode, message)
		close(notifications)
		return nil
	}

	connection.SetCloseHandler(close)

	api.registerClient(connection, func(update StatusUpdate) {
		notifications <- update
	}, close)

	err = api.writeJSONMessage(connection, &StatusUpdate{Type: "connected", Extensions: api.Extensions})

	if err != nil {
		close(websocket.CloseNoStatusReceived, "cannot establish connection to client")
		return
	}

	go handleClientMessages(connection)

	for notification := range notifications {
		encoder := json.NewEncoder(rw)
		encoder.Encode(extensionsResponse{api.Extensions, api.Version})

		err = api.writeJSONMessage(connection, &notification)
		if err != nil {
			break
		}
	}
}

func (api *ExtensionsApi) listExtensions(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Add("Content-Type", "application/json")
	encoder := json.NewEncoder(rw)
	encoder.Encode(extensionsResponse{api.Extensions, api.Version})
}

func (api *ExtensionsApi) extensionRoot(rw http.ResponseWriter, r *http.Request) {
	log.Printf("request content headers: %v", r.Header.Get("accept"))
	rw.Header().Add("Content-Type", "text/html")

	requestUrl, err := url.Parse(r.RequestURI)

	if err != nil {
		rw.Write([]byte(fmt.Sprintf("not found: %v", err)))
		return
	}

	re := regexp.MustCompile(`^\/?extensions\/(?P<uuid>([a-z]|[0-9]|-)+)\/?`)
	matches := re.FindStringSubmatch(requestUrl.Path)
	uuidIndex := re.SubexpIndex("uuid")
	if uuidIndex < 0 {
		rw.Write([]byte("not found, extension has an invalid uuid"))
		return
	}

	uuid := matches[uuidIndex]
	extensionPath := matches[0]
	extension := api.Extensions[0]

	log.Printf("uuid: %v", uuid)

	log.Printf("scheme: %v", requestUrl.Scheme)
	// if extension.Type == "checkout-post-purchase" {

	// } else {
	// 	content, err := mergeTemplateWithData(&extensionTemplateData{&extension, api.Port, extensionPath}, filepath.Join("templates", "tunnel-error.html.tpl"))
	// 	if err != nil {
	// 		rw.Write([]byte(fmt.Sprintf("not found: %v", err)))
	// 		return
	// 	}
	// 	rw.Write(content.Bytes())
	// }
	rw.Write(api.handleTunnelError(&extension, extensionPath))
}

func (api *ExtensionsApi) handleTunnelError(extension *core.Extension, extensionPath string) []byte {
	content, err := api.getTunnelErrorContent(extension, extensionPath)

	if err != nil {
		return []byte(fmt.Sprintf("not found: %v", err))
	}

	return content.Bytes()
}

func (api *ExtensionsApi) getTunnelErrorContent(extension *core.Extension, extensionPath string) (*bytes.Buffer, error) {
	specificTemplatePath := filepath.Join("templates", "post-purchase", "tunnel-error.html.tpl")
	globalTemplatePath := filepath.Join("templates", "tunnel-error.html.tpl")

	targetFile, openErr := templates.Open(specificTemplatePath)

	if openErr != nil {
		return mergeTemplateWithData(&extensionTemplateData{extension, api.Port, extensionPath}, globalTemplatePath)
	}
	defer targetFile.Close()
	return mergeTemplateWithData(&extensionTemplateData{extension, api.Port, extensionPath}, specificTemplatePath)
}

func mergeTemplateWithData(extension *extensionTemplateData, filePath string) (*bytes.Buffer, error) {
	var templateContent bytes.Buffer
	content, err := templates.ReadFile(filePath)
	if err != nil {
		return &templateContent, err
	}

	fileTemplate := template.New(filePath)
	fileTemplate, err = fileTemplate.Parse(string(content))
	if err != nil {
		return &templateContent, err
	}

	if err = fileTemplate.Execute(&templateContent, extension); err != nil {
		return &templateContent, err
	}

	return &templateContent, nil
}

func (api *ExtensionsApi) registerClient(connection *websocket.Conn, notify notificationHandler, close closeHandler) bool {
	api.connections.Store(connection, client{notify, close})
	return true
}

func (api *ExtensionsApi) unregisterClient(connection *websocket.Conn, closeCode int, message string) {
	duration := 1 * time.Second
	deadline := time.Now().Add(duration)

	connection.SetWriteDeadline(deadline)
	connection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(closeCode, message))

	// TODO: Break out of this 1 second wait if the client responds correctly to the close message
	<-time.After(duration)
	connection.Close()
	api.connections.Delete(connection)
}

func (api *ExtensionsApi) writeJSONMessage(connection *websocket.Conn, statusUpdate *StatusUpdate) error {
	connection.SetWriteDeadline(time.Now().Add(1 * time.Second))
	return connection.WriteJSON(statusUpdate)
}

func handleClientMessages(connection *websocket.Conn) {
	// TODO: Handle messages from the client
	// Currently we don't do anything with the messages
	// but the code is needed to establish a two-way connection
	for {
		_, _, err := connection.ReadMessage()
		if err != nil {
			break
		}
	}
}

type ExtensionsApi struct {
	*core.ExtensionService
	*mux.Router
	connections sync.Map
	fs          *fsutils.FS
}

type StatusUpdate struct {
	Type       string           `json:"type"`
	Extensions []core.Extension `json:"extensions"`
}

type extensionsResponse struct {
	Extensions []core.Extension `json:"extensions"`
	Version    string           `json:"version"`
}

type client struct {
	notify notificationHandler
	close  closeHandler
}

type notificationHandler func(StatusUpdate)

type closeHandler func(code int, text string) error

type extensionTemplateData struct {
	*core.Extension
	Port          int
	ExtensionPath string
}
