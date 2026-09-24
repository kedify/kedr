package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/muesli/cancelreader"
)

// ChooseMetricsEndpoint writes its prompt to stderr so redirected reports stay
// clean. An explicit choice is required when discovery returns alternatives.
func ChooseMetricsEndpoint(ctx context.Context, contextName string, options []string) (int, error) {
	return chooseMetricsEndpoint(ctx, contextName, options, os.Stdin, os.Stderr, IsTerminal(os.Stdin) && IsTerminal(os.Stderr))
}

func chooseMetricsEndpoint(ctx context.Context, contextName string, options []string, in io.Reader, out io.Writer, interactive bool) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	if len(options) == 0 {
		return -1, errors.New("no metrics endpoints available; specify --prometheus-url")
	}
	if len(options) == 1 {
		return 0, nil
	}
	var menu strings.Builder
	fmt.Fprintf(&menu, "Multiple metrics endpoints found in Kubernetes context %q:\n", contextName)
	for i, option := range options {
		fmt.Fprintf(&menu, "  %d) %s\n", i+1, option)
	}
	if !interactive {
		return -1, fmt.Errorf("%sNo interactive terminal available. Select an endpoint with --prometheus-url <URL>", menu.String())
	}
	if _, err := io.WriteString(out, menu.String()); err != nil {
		return -1, err
	}
	reader, err := cancelreader.NewReader(in)
	if err != nil {
		return -1, fmt.Errorf("read endpoint selection: %w", err)
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		reader.Cancel()
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}
		_ = reader.Close()
	}()
	scanner := bufio.NewScanner(reader)
	for {
		if _, err := fmt.Fprintf(out, "Choose an endpoint [1-%d] (q to cancel): ", len(options)); err != nil {
			return -1, err
		}
		ok := scanner.Scan()
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		if !ok {
			if err := scanner.Err(); err != nil {
				return -1, fmt.Errorf("read endpoint selection: %w", err)
			}
			return -1, errors.New("no metrics endpoint selected; specify --prometheus-url")
		}
		choice := strings.TrimSpace(scanner.Text())
		if strings.EqualFold(choice, "q") {
			return -1, errors.New("metrics endpoint selection cancelled")
		}
		number, err := strconv.Atoi(choice)
		if err == nil && number >= 1 && number <= len(options) {
			return number - 1, nil
		}
		if _, err := fmt.Fprintf(out, "Enter a number from 1 to %d, or q to cancel.\n", len(options)); err != nil {
			return -1, err
		}
	}
}
