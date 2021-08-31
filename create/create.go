package create

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Shopify/shopify-cli-extensions/core"
	"github.com/Shopify/shopify-cli-extensions/create/fsutils"
)

//go:embed templates/* templates/.shopify-cli.yml.tpl
var templates embed.FS
var templateRoot = "templates"
var templateFileExtension = ".tpl"
var defaultSourceDir = "src"

func NewExtensionProject(extension core.Extension) (err error) {
	fs := fsutils.NewFS(&templates, templateRoot)
	project := &project{
		&extension,
		strings.ToUpper(extension.Type),
		strings.Contains(extension.Development.Template, "react"),
		strings.Contains(extension.Development.Template, "typescript"),
	}

	setup := NewProcess(
		MakeDir(extension.Development.RootDir),
		CreateSourceFiles(fs, project),
		MergeTemplates(fs, project),
		MergeYamlAndJsonFiles(fs, project),
	)

	return setup.Run()
}

type Task struct {
	Run  func() error
	Undo func() error
}

func NewProcess(tasks ...Task) Process {
	return Process{
		tasks:  tasks,
		status: make([]string, len(tasks)),
	}
}

func MakeDir(path string) Task {
	return Task{
		Run: func() error {
			return fsutils.MakeDir(path)
		},
		Undo: func() error {
			return fsutils.RemoveDir(path)
		},
	}
}

func CreateSourceFiles(fs *fsutils.FS, project *project) Task {
	sourceDirPath := filepath.Join(project.Development.RootDir, defaultSourceDir)

	return Task{
		Run: func() error {
			if err := fsutils.MakeDir(sourceDirPath); err != nil {
				return err
			}

			project.Development.Entries = make(map[string]string)
			project.Development.Entries["main"] = filepath.Join(defaultSourceDir, getMainFileName(project))

			err := fs.CopyFile(
				filepath.Join(project.Type, getMainTemplate(project)),
				filepath.Join(project.Development.RootDir, project.Development.Entries["main"]),
			)

			if err != nil {
				return err
			}

			templateSourceDir := filepath.Join(project.Type, defaultSourceDir)
			return fs.Execute(&fsutils.Operation{
				SourceDir: templateSourceDir,
				TargetDir: sourceDirPath,
				OnEachFile: func(filePath, targetPath string) (err error) {
					return fs.CopyFile(
						filePath,
						targetPath,
					)
				},
				SkipEmpty: true,
			})
		},
		Undo: func() error {
			return fsutils.RemoveDir(sourceDirPath)
		},
	}
}

func MergeTemplates(fs *fsutils.FS, project *project) Task {
	newFilePaths := make([]string, 0)
	return Task{
		Run: func() error {
			return fs.Execute(&fsutils.Operation{
				SourceDir: "",
				TargetDir: project.Development.RootDir,
				OnEachFile: func(filePath, targetPath string) (err error) {
					if !strings.HasSuffix(targetPath, templateFileExtension) {
						return
					}

					targetFilePath := strings.TrimSuffix(targetPath, templateFileExtension)

					content, err := mergeTemplateWithData(project, filePath)
					if err != nil {
						return
					}

					formattedContent, err := fsutils.FormatContent(targetFilePath, content.Bytes())
					if err != nil {
						return
					}
					newFilePaths = append(newFilePaths, targetFilePath)
					return fsutils.CopyFileContent(targetFilePath, formattedContent)
				},
				SkipEmpty: false,
			})
		},
		Undo: func() (err error) {
			for _, filePath := range newFilePaths {
				if err = os.Remove(filePath); err != nil {
					return
				}
			}
			return
		},
	}
}

type files struct {
	content  []byte
	filePath string
}

type packageJSON struct {
	DevDependencies map[string]string `json:"devDependencies"`
	Dependencies    map[string]string `json:"dependencies"`
	License         string            `json:"license"`
	Scripts         map[string]string `json:"scripts"`
}

func MergeYamlAndJsonFiles(fs *fsutils.FS, project *project) Task {
	filesToRestore := make([]files, 0)
	return Task{
		Run: func() error {
			return fs.Execute(&fsutils.Operation{
				SourceDir: project.Type,
				TargetDir: project.Development.RootDir,
				OnEachFile: func(filePath, targetPath string) (err error) {
					if !strings.HasSuffix(targetPath, ".yml") && !strings.HasSuffix(targetPath, ".json") {
						return
					}

					targetFile, openErr := fsutils.OpenFileForAppend(targetPath)

					if openErr != nil {
						return fs.CopyFile(filePath, targetPath)
					}

					defer targetFile.Close()

					content, err := templates.ReadFile(filePath)
					if err != nil {
						return
					}

					oldContent, err := os.ReadFile(targetPath)

					if err != nil {
						return
					}

					filesToRestore = append(filesToRestore, files{oldContent, targetPath})

					var formattedContent []byte

					if strings.HasSuffix(targetPath, ".yml") {
						formattedContent, err = mergeYaml(oldContent, content, fs)
					} else if strings.HasSuffix(targetPath, ".json") {
						formattedContent, err = mergeJson(oldContent, content, fs)
					} else {
						fmt.Printf("filePath, targetPath, %v, %v", filePath, targetPath)
					}

					if err = os.WriteFile(targetPath, formattedContent, 0600); err != nil {
						return
					}

					return
				},
				SkipEmpty: false,
			})
		},
		Undo: func() (err error) {
			for _, file := range filesToRestore {
				return os.WriteFile(file.filePath, file.content, 0600)
			}
			return
		},
	}
}

func mergeYaml(originalContent []byte, newContent []byte, fs *fsutils.FS) (formattedContent []byte, err error) {
	originalStr := string(originalContent)
	newStr := strings.Replace(string(newContent), "---", "", 1)

	formattedContent = []byte(originalStr)
	formattedContent = append(formattedContent, []byte(newStr)...)
	return
}

func mergeJson(originalContent []byte, newContent []byte, fs *fsutils.FS) (formattedContent []byte, err error) {
	var result packageJSON
	var newResult packageJSON
	if err = json.Unmarshal(originalContent, &result); err != nil {
		return
	}

	if err = json.Unmarshal(newContent, &newResult); err != nil {
		return
	}

	for k, v := range newResult.Dependencies {
		result.Dependencies[k] = v
	}
	for k, v := range newResult.DevDependencies {
		result.DevDependencies[k] = v
	}

	newContent, err = json.Marshal(result)

	if err != nil {
		return
	}

	formattedContent, err = fsutils.FormatJSON(newContent)

	if err != nil {
		return
	}

	return
}

type Process struct {
	tasks  []Task
	status []string
}

func (p *Process) Run() (err error) {
	for taskId, task := range p.tasks {
		if err = task.Run(); err != nil {
			p.status[taskId] = "fail"
			if undoErr := p.Undo(); undoErr != nil {
				fmt.Printf("Failed to undo with error: %v\n", undoErr)
			}
			return
		}
		p.status[taskId] = "success"
	}
	return
}

func (p *Process) Undo() (err error) {
	for taskId := range p.status {
		taskId = len(p.status) - 1 - taskId
		if p.status[taskId] == "fail" {
			return p.tasks[taskId].Undo()
		}
	}
	return
}

func mergeTemplateWithData(project *project, filePath string) (*bytes.Buffer, error) {
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

	if err = fileTemplate.Execute(&templateContent, project); err != nil {
		return &templateContent, err
	}

	return &templateContent, nil
}

func getMainFileName(project *project) string {
	if project.React && project.TypeScript {
		return "index.tsx"
	}

	if project.TypeScript {
		return "index.ts"
	}

	return "index.js"
}

func getMainTemplate(project *project) string {
	if project.React {
		return "react.js"
	}
	return "javascript.js"
}

type project struct {
	*core.Extension
	FormattedType string
	React         bool
	TypeScript    bool
}
