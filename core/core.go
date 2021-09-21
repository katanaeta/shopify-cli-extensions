package core

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

func NewExtensionService(config *Config, apiRoot string) *ExtensionService {
	extensions := config.Extensions
	host := fmt.Sprintf("http://%s:%d", "localhost", config.Port)
	if config.PublicUrl != "" {
		host = config.PublicUrl
	}

	apiUrl := fmt.Sprintf("%s%s", host, apiRoot)

	for index, extension := range extensions {
		keys := make([]string, 0, len(extensions[index].Development.Entries))
		for key := range extensions[index].Development.Entries {
			keys = append(keys, key)
		}

		extensions[index].Assets = make(map[string]Asset)

		for entry := range keys {
			name := keys[entry]
			extensionRoot := fmt.Sprintf("%s%s", apiUrl, extension.UUID)
			extensions[index].Development.Root.Url = extensionRoot
			extensions[index].Assets[name] = Asset{Url: fmt.Sprintf("%s/assets/%s.js", extensionRoot, name), Name: name}
		}

		extensions[index].App = make(App)
	}

	service := ExtensionService{
		Version:    "0.1.0",
		Extensions: extensions,
		Port:       config.Port,
		PublicUrl:  config.PublicUrl,
		Store:      config.Store,
		ApiUrl:     apiUrl,
	}

	return &service
}

func LoadConfig(r io.Reader) (config *Config, err error) {
	config = &Config{}
	decoder := yaml.NewDecoder(r)
	err = decoder.Decode(config)
	return
}

type Config struct {
	Extensions []Extension `yaml:"extensions"`
	Port       int
	Store      string
	PublicUrl  string `yaml:"public_url"`
}

type ExtensionService struct {
	Extensions []Extension
	Version    string
	Port       int
	Store      string
	PublicUrl  string
	ApiUrl     string
}

type Extension struct {
	Type        string           `json:"type" yaml:"type"`
	UUID        string           `json:"uuid" yaml:"uuid"`
	Assets      map[string]Asset `json:"assets" yaml:"-"`
	Development Development      `json:"development" yaml:"development"`
	User        User             `json:"user" yaml:"user"`
	App         App              `json:"app" yaml:"-"`
	Version     string           `json:"version" yaml:"version"`
}

type Asset struct {
	Name string `json:"name" yaml:"name"`
	Url  string `json:"url" yaml:"url"`
}

type Development struct {
	Root     Url               `json:"root"`
	Resource Url               `json:"resource"`
	Renderer Renderer          `json:"-" yaml:"renderer"`
	Hidden   bool              `json:"hidden"`
	Focused  bool              `json:"focused"`
	BuildDir string            `json:"-" yaml:"build_dir"`
	RootDir  string            `json:"-" yaml:"root_dir"`
	Template string            `json:"-"`
	Entries  map[string]string `json:"-"`
}

type Renderer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type User struct {
	Metafields []Metafield `json:"metafields" yaml:"metafields"`
}

type Metafield struct {
	Namespace string `json:"namespace" yaml:"namespace"`
	Key       string `json:"key" yaml:"key"`
}

type App map[string]interface{}

type Url struct {
	Url string `json:"url" yaml:"url"`
}
