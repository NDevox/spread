package cli

import (
	"github.com/codegangsta/cli"

	"rsprd.com/spread/pkg/data"
	pb "rsprd.com/spread/pkg/spreadproto"
)

// Param allows the parameters to be created on the Index
func (s SpreadCli) Param() *cli.Command {
	return &cli.Command{
		Name:      "param",
		Usage:     "Set paramaters for field values in the index",
		ArgsUsage: "<SRL> <name> <prompt>",
		Flags: []cli.Flag{
			cli.BoolFlag{
				Name:  "l",
				Usage: "list parameters",
			},
			cli.StringFlag{
				Name:  "f",
				Usage: "set Golang format string to use with arguments",
			},
			cli.StringFlag{
				Name:  "d",
				Usage: "set default value, interpretted as JSON",
			},
		},
		Action: func(c *cli.Context) {
			if c.Bool("l") {
				p := s.projectOrDie()
				docs, err := p.Index()
				if err != nil {
					s.fatalf("Could not retrieve index: %v", err)
				}

				paramFields := data.ParameterFields(docs)
				for _, field := range paramFields {
					param := field.GetParam()
					s.printf(" - Name: %s", param.Name)
					s.printf("   Description: %s", param.Prompt)
					s.printf("   Pattern: %s", param.Pattern)
					if param.GetDefault() == nil {
						s.printf("   Required: Yes")

					}
				}
				return
			}

			if len(c.Args()) < 3 {
				s.fatalf("an srl, name, and description must be provided")
			}

			targetUrl := c.Args().First()
			if len(targetUrl) == 0 {
				s.fatalf("A target SRI must be specified")
			}

			target, err := data.ParseSRI(targetUrl)
			if err != nil {
				s.fatalf("Error using target: %v", err)
			}

			proj := s.projectOrDie()
			doc, err := proj.DocFromIndex(target.Path)
			if err != nil {
				s.fatalf("Error retrieving from index: %v", err)
			}

			param := &pb.Parameter{
				Name:    c.Args().Get(1),
				Prompt:  c.Args().Get(2),
				Pattern: c.String("f"),
			}

			// parse default value
			defaultInput := c.String("d")
			if len(defaultInput) != 0 {
				args, err := data.ParseArguments(defaultInput, false)
				if err != nil {
					s.fatalf("Could not parse default value: %v", err)
				} else if len(args) > 1 {
					s.fatalf("Only one default value can be specified")
				}
				param.Default = args[0]
			}

			if err = data.AddParamToDoc(doc, target, param); err != nil {
				s.fatalf("Failed to add parameter: %v", err)
			}

			if err = proj.AddDocumentToIndex(doc); err != nil {
				s.fatalf("Failed to add object to Git index: %v", err)
			}
		},
	}
}
