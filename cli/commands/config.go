// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This specific file handles the CLI commands that interact with configurations of AIS daemons
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cli/templates"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/urfave/cli"
)

var (
	configFlags = map[string][]cli.Flag{
		configGet: {jsonFlag},
		configSet: {},
	}

	configCmds = []cli.Command{
		{
			Name:  commandConfig,
			Usage: "interact with daemon configs",
			Flags: []cli.Flag{},
			Subcommands: []cli.Command{
				{
					Name:         configGet,
					Usage:        "displays configuration of a daemon",
					ArgsUsage:    daemonIDArgumentText,
					Action:       configHandler,
					Flags:        configFlags[configGet],
					BashComplete: daemonConfigSectionSuggestions(false /* optional daemon */, true /* optional config */),
				},
				{
					Name:         configSet,
					Usage:        "updates configuration of a single node or the entire cluster",
					ArgsUsage:    configSetArgumentText,
					Action:       configHandler,
					Flags:        configFlags[configSet],
					BashComplete: setConfigSuggestions,
				},
			},
		},
	}
)

func configHandler(c *cli.Context) error {
	if _, err := fillMap(ClusterURL); err != nil {
		return err
	}

	var (
		err error

		baseParams = cliAPIParams(ClusterURL)
		command    = c.Command.Name
	)

	switch command {
	case configGet:
		err = getConfig(c, baseParams)
	case configSet:
		err = setConfig(c, baseParams)
	default:
		return fmt.Errorf(invalidCmdMsg, command)
	}

	return err
}

// Displays the config of a daemon
func getConfig(c *cli.Context, baseParams *api.BaseParams) error {
	var (
		daemonID = c.Args().Get(0)
		section  = c.Args().Get(1)
		useJSON  = flagIsSet(c, jsonFlag)
	)

	if c.NArg() == 0 {
		return missingArgumentsError(c, "daemon ID")
	}

	body, err := api.GetDaemonConfig(baseParams, daemonID)
	if err != nil {
		return err
	}

	template := templates.ConfigTmpl
	if section != "" {
		if t, ok := templates.ConfigSectionTmpl[section]; ok {
			template = strings.TrimPrefix(t, "\n")
		} else {
			return fmt.Errorf("config section %q not found", section)
		}
	}

	return templates.DisplayOutput(body, c.App.Writer, template, useJSON)
}

// Sets config of specific daemon or cluster
func setConfig(c *cli.Context, baseParams *api.BaseParams) error {
	daemonID, nvs, err := daemonKeyValueArgs(c)
	if err != nil {
		return err
	}

	if daemonID == "" {
		if err := api.SetClusterConfig(baseParams, nvs); err != nil {
			return err
		}

		fmt.Fprintf(c.App.Writer, "Config has been updated successfully.\n")
		return nil
	}

	if err := api.SetDaemonConfig(baseParams, daemonID, nvs); err != nil {
		return err
	}

	fmt.Fprintf(c.App.Writer, "Config for node %q has been updated successfully.\n", daemonID)
	return nil
}

func daemonKeyValueArgs(c *cli.Context) (daemonID string, nvs cmn.SimpleKVs, err error) {
	if c.NArg() == 0 {
		return "", nil, missingArgumentsError(c, "attribute key-value pairs")
	}

	args := c.Args()
	daemonID = args.First()
	kvs := args.Tail()

	// Case when DAEMON_ID is not provided by the user:
	// 1. Key-value pair separated with '=': `ais set log.level=5`
	// 2. Key-value pair separated with space: `ais set log.level 5`. In this case
	//		the first word is looked up in cmn.ConfigPropList
	_, isProperty := cmn.ConfigPropList[args.First()]
	if isProperty || strings.Contains(args.First(), keyAndValueSeparator) {
		daemonID = ""
		kvs = args
	}

	if len(kvs) == 0 {
		return "", nil, missingArgumentsError(c, "attribute key-value pairs")
	}

	nvs, err = makePairs(kvs)
	if err != nil {
		return "", nil, err
	}

	return daemonID, nvs, nil
}
