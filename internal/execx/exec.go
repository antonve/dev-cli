package execx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Runner interface {
	Run(context.Context, string, []string, io.Reader) ([]byte, error)
	Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error
}

type OS struct{}

func (OS) Run(ctx context.Context, name string, args []string, in io.Reader) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Stdin = in
	var out, stderr bytes.Buffer
	c.Stdout = &out
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

func (OS) Stream(ctx context.Context, name string, args []string, in io.Reader, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Stdin = in
	c.Stdout = stdout
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func Exists(name string) bool { _, err := exec.LookPath(name); return err == nil }
func EnvOwner() string        { return os.Getenv("DEV_OWNER") }
