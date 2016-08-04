package cli

import (
	"fmt"
	"strings"

	"github.com/codegangsta/cli"
)

// Push allows references to be pushed to a remote.
func (s SpreadCli) Push() *cli.Command {
	return &cli.Command{
		Name:        "push",
		Usage:       "Push references to a remote",
		ArgsUsage:   "<remote> <refspec>",
		Description: "Push Spread data to a remote",
		Action: func(c *cli.Context) {
			remoteName := c.Args().First()
			if len(remoteName) == 0 {
				s.fatalf("a remote must be specified")
			}

			if len(c.Args()) < 2 {
				s.fatalf("a refspec must be specified")
			}

			refspecs := c.Args()[1:]

			for i, spec := range refspecs {
				if !strings.HasPrefix(spec, "refs/") {
					refspecs[i] = fmt.Sprintf("refs/heads/%s", spec)
				}
			}

			p := s.projectOrDie()
			err := p.Push(remoteName, refspecs...)
			if err != nil {
				s.fatalf("Failed to push: %v", err)
			}
		},
	}
}
