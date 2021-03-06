package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	calculationsv1 "gitlab.physics.muni.cz/vega-project/ccb-operator/pkg/apis/calculations/v1"
)

const (
	teffVar        = "Teff"
	logGVar        = "LogG"
	fort95Filename = "fort.95"
)

// Executor ...
type Executor struct {
	logger                   *logrus.Entry
	executeChan              chan *calculationsv1.Calculation // Replace it with steps struct
	stepUpdaterChan          chan Result
	Status                   string
	nfsPath                  string
	atlasControlFiles        string
	atlasDataFiles           string
	kuruzModelTemplateFile   string
	synspecInputTemplateFile string
}

// Result ...
type Result struct {
	CalcName     string
	Step         int
	Status       calculationsv1.CalculationPhase
	StdoutStderr string
	CommandError error
}

// NewExecutor ...
func NewExecutor(executeChan chan *calculationsv1.Calculation, stepUpdaterChan chan Result, nfsPath, atlasControlFiles, atlasDataFiles, kuruzModelTemplateFile, synspecInputTemplateFile string) *Executor {
	return &Executor{
		executeChan:              executeChan,
		stepUpdaterChan:          stepUpdaterChan,
		nfsPath:                  nfsPath,
		atlasControlFiles:        atlasControlFiles,
		atlasDataFiles:           atlasDataFiles,
		kuruzModelTemplateFile:   kuruzModelTemplateFile,
		synspecInputTemplateFile: synspecInputTemplateFile,
	}
}

// TODO: input filenames --> flags or const
// Run ...
func (e *Executor) Run() {
	for {
		select {
		case calc := <-e.executeChan:
			e.logger = logrus.WithField("for-calculation", calc.Name)

			// TODO: Can this run only once when the worker is starting????????????
			// Setting stack limit
			if err := setUnlimitStack(); err != nil {
				e.logger.WithError(err).Error("couln't set stack limit")
				break
			}

			// Creating folder
			calcPath := filepath.Join(e.nfsPath, calc.Name)
			if _, err := os.Stat(calcPath); err != nil {
				if err := os.MkdirAll(calcPath, os.ModePerm); err != nil {
					e.logger.WithError(err).Error("couln't create directory. Aborting...")
					break
				}
			}

			// Creating symbolic links with the data/control files for atlas12_ada
			if err := e.createSymbolicLinks([]string{e.atlasControlFiles, e.atlasDataFiles}, calcPath); err != nil {
				break
			}

			// Running steps
			// We want only one step to run each time.
			execution := createExecution(calc.Name, calc.Spec.Steps)
			for index, step := range execution.steps {

				// TODO make this more selective.
				if len(step.status) != 0 {
					continue
				}

				if index == 0 {
					// Generate the input file
					if err := e.generateInputFile(filepath.Join(calcPath, e.kuruzModelTemplateFile),
						filepath.Join(calcPath, "t10000_400_72.mod.7011870916"), calc.Spec.Teff, calc.Spec.LogG); err != nil {
						e.logger.WithError(err).Error("couldn't generate the input file")
						break
					}
				}

				if index == 2 {
					e.logger.Info("Generate Synspec Input files...")
					if contents, err := generateSynspecInputFile(calcPath, "t10000_400_72_strat.mod", "fort.8"); err != nil {
						e.logger.WithError(err).Error("couldn't generate the Synspec's input file")
						break
					} else {
						if err := ioutil.WriteFile(filepath.Join(calcPath, "fort.8"), contents, 0777); err != nil {
							e.logger.WithError(err).Error("couldn't generate the new input file")
							break
						}
					}

					if err := generateSynspecInputRuntimeFile(calcPath, e.synspecInputTemplateFile, "input_tlusty_fortfive", calc.Spec.Teff, calc.Spec.LogG); err != nil {
						e.logger.WithError(err).Error("couldn't generate the Synspec's Runtime input file")
						break
					}

				}

				var status calculationsv1.CalculationPhase
				var cmdErr error
				status = "Completed"

				// TODO test this :)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()

				cmd := exec.CommandContext(ctx, step.command, strings.Join(step.args, " "))
				cmd.Dir = calcPath

				fields := logrus.Fields{"command": cmd.Args, "step": index}
				e.logger.WithFields(fields).Info("Running command and waiting for it to finish...")

				combinedOut, err := cmd.CombinedOutput()
				if err != nil {
					e.logger.WithError(err).WithField("output", string(combinedOut)).Error("command failed...")
					status = "Failed"
					cmdErr = err
				}

				result := Result{
					CalcName:     calc.Name,
					Step:         index,
					StdoutStderr: string(combinedOut),
					Status:       status,
					CommandError: cmdErr,
				}

				e.logger.WithFields(fields).WithField("status", status).Info("Command finished")
				e.stepUpdaterChan <- result
			}
		}
	}
}

// generateInputFile generates the input file to be used by Atlas12
func (e *Executor) generateInputFile(file, outFile string, teff, logG float64) error {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return fmt.Errorf("could not read file %q: %v", file, err)
	}

	vars := make(map[string]interface{})
	vars[teffVar] = fmt.Sprintf("%.1f", teff)
	vars[logGVar] = fmt.Sprintf("%.2f", logG)

	contents, err := parseTemplate(data, vars)
	if err != nil {
		return err
	}

	e.logger.WithField("filename", outFile).Info("Generating input file...")
	if err := ioutil.WriteFile(outFile, contents, 0777); err != nil {
		return fmt.Errorf("couldn't generate the new input file: %v", err)
	}

	return nil
}

func (e *Executor) createSymbolicLinks(paths []string, toPath string) error {
	for _, path := range paths {
		err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				e.logger.WithError(err).Errorf("prevent panic by handling failure accessing a path %q", path)
				return err
			}
			if !info.IsDir() {
				symbTarget := filepath.Join(toPath, filepath.Base(path))
				os.Symlink(path, symbTarget)
			}
			return nil
		})
		if err != nil {
			e.logger.WithField("path", path).WithError(err).Error("error while walking to path")
			return err
		}
	}
	return nil
}

type execution struct {
	calculationName string
	steps           []step
}

type step struct {
	command string
	args    []string
	status  calculationsv1.CalculationPhase
}

func createExecution(calcName string, steps []calculationsv1.Step) *execution {
	execution := &execution{
		calculationName: calcName,
	}

	for _, calcStep := range steps {
		s := step{
			command: calcStep.Command,
			args:    calcStep.Args,
			status:  calcStep.Status,
		}
		execution.steps = append(execution.steps, s)
	}

	return execution
}

func setUnlimitStack() error {
	var rLimit unix.Rlimit
	rLimit.Max = 18446744073709551615
	rLimit.Cur = 18446744073709551615

	if err := unix.Setrlimit(unix.RLIMIT_STACK, &rLimit); err != nil {
		return fmt.Errorf("Error Setting Rlimit %v", err)
	}
	return nil
}

func generateSynspecInputFile(path, modPrefix, outFile string) ([]byte, error) {
	var contents string
	space := regexp.MustCompile(`\s+`)

	err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() && strings.HasPrefix(filepath.Base(path), modPrefix) {
			file, err := os.Open(path)
			if err != nil {
				log.Fatal(err)
			}
			defer file.Close()

			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := scanner.Text()
				var toAppend string

				toAppend = space.ReplaceAllString(line, " ")
				if strings.HasPrefix(toAppend, "TEFF") {
					toAppend = recreateVarsLine(strings.Split(toAppend, " "))
				} else if strings.HasPrefix(toAppend, "READ DECK6 72") {
					toAppend = strings.Replace(toAppend, "READ DECK6 72", "READ DECK6 64", -1)
				}
				contents += toAppend + "\n"
			}

			if err := scanner.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error while walking to path")
	}

	return []byte(contents), nil
}

// In order to make synspec49 to be able to read the file, we need to do this ugly hack.
func recreateVarsLine(lineValues []string) string {
	// Return line for Synspec format
	if len(lineValues[1]) == 6 {
		// 2 space, 1 space, 2 spaces, 3 spaces
		return fmt.Sprintf("%s  %s %s  %s   %s",
			lineValues[0],
			lineValues[1],
			lineValues[2],
			lineValues[3],
			lineValues[4])
	}
	// 1 space, 1 space, 2 spaces, 3 spaces
	return fmt.Sprintf("%s %s %s  %s   %s",
		lineValues[0],
		lineValues[1],
		lineValues[2],
		lineValues[3],
		lineValues[4])
}

func generateSynspecInputRuntimeFile(calcPath, templateFile, outFile string, teff, logG float64) error {
	template := filepath.Join(calcPath, templateFile)
	fort95File := filepath.Join(calcPath, fort95Filename)

	synspecInputFile := filepath.Join(calcPath, outFile)
	data, err := ioutil.ReadFile(template)
	if err != nil {
		return err
	}

	vars := make(map[string]interface{})
	vars[teffVar] = fmt.Sprintf("%.4f", teff)
	vars[logGVar] = fmt.Sprintf("%.4f", logG)

	contents, err := parseTemplate(data, vars)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(synspecInputFile, contents, 0777); err != nil {
		return fmt.Errorf("couldn't generate the new input file: %v", err)
	}

	if err := ioutil.WriteFile(fort95File, contents, 0777); err != nil {
		return fmt.Errorf("couldn't generate the new input file: %v", err)
	}

	return nil
}

func parseTemplate(data []byte, vars interface{}) ([]byte, error) {
	var tmplBytes bytes.Buffer

	tmpl, err := template.New("tmpl").Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("error while parsing the template's data: %v", err)
	}

	if err := tmpl.Execute(&tmplBytes, vars); err != nil {
		return nil, fmt.Errorf("error while executing the template: %v", err)
	}

	return tmplBytes.Bytes(), nil
}
