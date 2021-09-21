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
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/Shopify/shopify-cli-extensions/core"
	"github.com/Shopify/shopify-cli-extensions/create/fsutils"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

//go:embed templates/*
var templates embed.FS
var apiRoot = "/extensions/"

func New(config *core.Config) *ExtensionsApi {
	mux := mux.NewRouter().StrictSlash(true)
	fs := fsutils.NewFS(&templates, "templates")

	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, apiRoot, http.StatusTemporaryRedirect)
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

	api.HandleFunc(apiRoot, api.extensionsHandler)

	for _, extension := range api.Extensions {
		assets := path.Join(apiRoot, extension.UUID, "assets")
		buildDir := filepath.Join(".", extension.Development.RootDir, extension.Development.BuildDir)
		api.PathPrefix(assets).Handler(
			http.StripPrefix(assets, http.FileServer(http.Dir(buildDir))),
		)
	}

	api.HandleFunc(path.Join(apiRoot, "{uuid:(?:[a-z]|[0-9]|-)+}"), api.extensionRootHandler)

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

func (api *ExtensionsApi) extensionRootHandler(rw http.ResponseWriter, r *http.Request) {
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

	for _, extension := range api.Extensions {
		if extension.UUID == uuid {
			api.handleExtension(rw, r, &extension)
			return
		}
	}
}

func (api *ExtensionsApi) handleExtension(rw http.ResponseWriter, r *http.Request, extension *core.Extension) {
	host := fmt.Sprintf("http://%s", r.Host)

	if r.Host != "localhost" && api.PublicUrl != "" {
		host = api.PublicUrl
	}

	templateData := extensionTemplateData{
		extension,
		path.Join(host, apiRoot),
		api.Port,
		path.Join(host, apiRoot, extension.UUID),
		api.Store,
		getSurface(extension.Type),
	}

	log.Printf("templateData: %v", templateData)

	// TODO: Find a better way to handle this - looks like there's no easy way to get the request protocol
	if host == "http://localhost" {
		rw.Write(api.handleTunnelError(&templateData))
		return
	}

	content, err := api.getIndexContent(&templateData)
	if err != nil {
		rw.Write([]byte(fmt.Sprintf("not found: %v", err)))
		return
	}
	if content != nil {
		rw.Write(content.Bytes())
		return
	}

	api.handleExtensionRedirect(rw, r, &templateData)
}

func (api *ExtensionsApi) handleExtensionRedirect(rw http.ResponseWriter, r *http.Request, templateData *extensionTemplateData) {
	content, err := mergeTemplateWithData(templateData, "templates/info.yml.tpl")
	if err != nil {
		rw.Write([]byte(fmt.Sprintf("error: %v", err)))
		return
	}

	info := extensionInfo{}

	if err = yaml.Unmarshal(content.Bytes(), &info); err != nil {
		rw.Write([]byte(fmt.Sprintf("cannot read dat for extension: %v", err)))
		return
	}
	rw.Write([]byte(fmt.Sprintf("redirect: %v", info.RedirectUrl)))
	// http.Redirect(rw, r, info.RedirectUrl, http.StatusPermanentRedirect)
}

func (api *ExtensionsApi) handleTunnelError(templateData *extensionTemplateData) []byte {
	content, err := api.getTunnelErrorContent(templateData)

	if err != nil {
		return []byte(fmt.Sprintf("not found: %v", err))
	}

	return content.Bytes()
}

func (api *ExtensionsApi) getIndexContent(templateData *extensionTemplateData) (*bytes.Buffer, error) {
	specificTemplatePath := filepath.Join("templates", templateData.Type, "index.html.tpl")

	targetFile, err := templates.Open(specificTemplatePath)

	if err != nil {
		return nil, nil
	}

	defer targetFile.Close()
	return mergeTemplateWithData(templateData, specificTemplatePath)
}

func (api *ExtensionsApi) getTunnelErrorContent(templateData *extensionTemplateData) (*bytes.Buffer, error) {
	specificTemplatePath := filepath.Join("templates", templateData.Type, "tunnel-error.html.tpl")
	globalTemplatePath := filepath.Join("templates", "tunnel-error.html.tpl")

	targetFile, openErr := templates.Open(specificTemplatePath)

	if openErr != nil {
		return mergeTemplateWithData(templateData, globalTemplatePath)
	}
	defer targetFile.Close()
	return mergeTemplateWithData(templateData, specificTemplatePath)
}

func mergeTemplateWithData(templateData *extensionTemplateData, filePath string) (*bytes.Buffer, error) {
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

	if err = fileTemplate.Execute(&templateContent, templateData); err != nil {
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

func getSurface(extensionType string) string {
	if strings.Contains(extensionType, "checkout") {
		return "Checkout"
	}
	return "Admin"
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
	ApiUrl       string
	Port         int
	RelativePath string
	Store        string
	Surface      string
}

type extensionInfo struct {
	Messages struct {
		ExtensionUrl string `yaml:"extensions_url"`
		Login        string
	}
	RedirectUrl string `yaml:"redirect_url"`
}
