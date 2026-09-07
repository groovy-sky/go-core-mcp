package coreutils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Command is an in-process, stdin-only utility that can be safely exposed to
// an MCP client after its arguments have been validated.
type Command struct {
	Name         string
	Description  string
	ExposeToMCP  bool
	ReadOnly     bool
	ValidateArgs func([]string) error
	Run          func(context.Context, []string, io.Reader, io.Writer, io.Writer) error
}

// Commands returns the fixed registry of commands approved for MCP use.
func Commands() []Command {
	return []Command{
		textCommand("sort", "Sort newline-delimited text.", validateSort, runSort),
		textCommand("uniq", "Remove adjacent duplicate lines.", validateUniq, runUniq),
		textCommand("wc", "Count lines, words, or bytes.", validateWC, runWC),
		textCommand("tr", "Translate or delete characters.", validateTr, runTr),
		textCommand("head", "Select leading lines.", validateLineCount, runHead),
		textCommand("tail", "Select trailing lines.", validateLineCount, runTail),
		textCommand("cut", "Select delimiter-separated fields.", validateCut, runCut),
	}
}

func textCommand(name, description string, validate func([]string) error, run func(context.Context, []string, io.Reader, io.Writer, io.Writer) error) Command {
	return Command{Name: name, Description: description, ExposeToMCP: true, ReadOnly: true, ValidateArgs: validate, Run: run}
}

// LookupCommand returns an MCP-exposed command by its stable name.
func LookupCommand(name string) (Command, bool) {
	for _, command := range Commands() {
		if command.Name == name {
			return command, true
		}
	}
	return Command{}, false
}

func readText(ctx context.Context, input io.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return string(data), nil
}

func writeText(output io.Writer, text string) error {
	_, err := io.WriteString(output, text)
	return err
}

func validateSort(args []string) error {
	for _, arg := range args {
		if arg != "-r" && arg != "--reverse" && arg != "-n" && arg != "--numeric-sort" {
			return fmt.Errorf("unsupported sort argument %q", arg)
		}
	}
	return nil
}
func runSort(ctx context.Context, args []string, input io.Reader, output, _ io.Writer) error {
	text, err := readText(ctx, input)
	if err != nil {
		return err
	}
	reverse, numeric := false, false
	for _, arg := range args {
		reverse = reverse || arg == "-r" || arg == "--reverse"
		numeric = numeric || arg == "-n" || arg == "--numeric-sort"
	}
	return writeText(output, Sort(text, reverse, numeric, false))
}

func validateUniq(args []string) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "-c" || args[0] == "--count")) {
		return nil
	}
	return fmt.Errorf("unsupported uniq arguments")
}
func runUniq(ctx context.Context, args []string, input io.Reader, output, _ io.Writer) error {
	text, err := readText(ctx, input)
	if err != nil {
		return err
	}
	return writeText(output, Uniq(text, len(args) == 1))
}

func validateWC(args []string) error {
	for _, arg := range args {
		if arg != "-l" && arg != "-w" && arg != "-c" {
			return fmt.Errorf("unsupported wc argument %q", arg)
		}
	}
	return nil
}
func runWC(ctx context.Context, args []string, input io.Reader, output, _ io.Writer) error {
	text, err := readText(ctx, input)
	if err != nil {
		return err
	}
	counts := WordCount(text)
	if len(args) == 0 {
		return writeText(output, fmt.Sprintf("%d %d %d\n", counts.Lines, counts.Words, counts.Bytes))
	}
	values := make([]string, 0, len(args))
	for _, arg := range args {
		switch arg {
		case "-l":
			values = append(values, strconv.Itoa(counts.Lines))
		case "-w":
			values = append(values, strconv.Itoa(counts.Words))
		case "-c":
			values = append(values, strconv.Itoa(counts.Bytes))
		}
	}
	return writeText(output, strings.Join(values, " ")+"\n")
}

func validateTr(args []string) error {
	if len(args) == 2 || (len(args) == 2 && args[0] == "-d") {
		return nil
	}
	return errors.New("tr requires FROM TO, or -d FROM")
}
func runTr(ctx context.Context, args []string, input io.Reader, output, _ io.Writer) error {
	text, err := readText(ctx, input)
	if err != nil {
		return err
	}
	if args[0] == "-d" {
		result, err := Tr(text, args[1], "", true)
		if err != nil {
			return err
		}
		return writeText(output, result)
	}
	result, err := Tr(text, args[0], args[1], false)
	if err != nil {
		return err
	}
	return writeText(output, result)
}

func validateLineCount(args []string) error {
	if len(args) != 2 || args[0] != "-n" {
		return errors.New("requires -n COUNT")
	}
	count, err := strconv.Atoi(args[1])
	if err != nil || count < 0 || count > 10000 {
		return errors.New("count must be between 0 and 10000")
	}
	return nil
}
func runHead(ctx context.Context, args []string, input io.Reader, output, _ io.Writer) error {
	text, err := readText(ctx, input)
	if err != nil {
		return err
	}
	count, _ := strconv.Atoi(args[1])
	result, _ := Head(text, count)
	return writeText(output, result)
}
func runTail(ctx context.Context, args []string, input io.Reader, output, _ io.Writer) error {
	text, err := readText(ctx, input)
	if err != nil {
		return err
	}
	count, _ := strconv.Atoi(args[1])
	result, _ := Tail(text, count)
	return writeText(output, result)
}

func validateCut(args []string) error {
	if len(args) != 4 || args[0] != "-d" || args[2] != "-f" || args[1] == "" {
		return errors.New("requires -d DELIMITER -f FIELDS")
	}
	for _, field := range strings.Split(args[3], ",") {
		value, err := strconv.Atoi(field)
		if err != nil || value < 1 || value > 1024 {
			return errors.New("fields must be positive integers up to 1024")
		}
	}
	return nil
}
func runCut(ctx context.Context, args []string, input io.Reader, output, _ io.Writer) error {
	text, err := readText(ctx, input)
	if err != nil {
		return err
	}
	fields := strings.Split(args[3], ",")
	selected := make([]int, 0, len(fields))
	for _, field := range fields {
		value, _ := strconv.Atoi(field)
		selected = append(selected, value)
	}
	result, err := Cut(text, args[1], selected)
	if err != nil {
		return err
	}
	return writeText(output, result)
}
