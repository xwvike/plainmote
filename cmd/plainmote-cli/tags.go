package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
)

// tagFlags collects a flag given more than once: --tag a --tag b.
type tagFlags []string

func (t *tagFlags) String() string { return strings.Join(*t, ",") }

func (t *tagFlags) Set(value string) error {
	*t = append(*t, value)
	return nil
}

// tagCommand puts tags on a resource, or with remove takes them off.
func (c *cli) tagCommand(ctx context.Context, args []string, remove bool) error {
	command := "tag"
	if remove {
		command = "untag"
	}
	args, server, err := c.flags(command, args, nil)
	if err != nil {
		return err
	}
	if len(args) < 2 {
		return c.needArgs(args, 2, command, "<resource> <tag>...")
	}
	_, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	target, err := api.resolve(ctx, args[0])
	if err != nil {
		return err
	}
	var tagged resource
	if remove {
		tagged, err = api.retag(ctx, target.ID, nil, args[1:])
	} else {
		tagged, err = api.retag(ctx, target.ID, args[1:], nil)
	}
	if err != nil {
		return err
	}
	c.sayTags(target, tagged.Tags)
	return nil
}

func (c *cli) sayTags(target resource, tags []string) {
	if len(tags) == 0 {
		fmt.Fprintln(c.stdout, msg("tag_none", target.label()))
		return
	}
	fmt.Fprintln(c.stdout, msg("tag_done", target.label(), tagText(tags)))
}

func tagText(tags []string) string {
	cleaned := make([]string, len(tags))
	for i, tag := range tags {
		cleaned[i] = clean(tag)
	}
	return strings.Join(cleaned, ", ")
}

// tagsCommand lists the account's tags with how many resources carry each.
func (c *cli) tagsCommand(ctx context.Context, args []string) error {
	args, server, err := c.flags("tags", args, nil)
	if err != nil {
		return err
	}
	if err := c.needArgs(args, 0, "tags", "no arguments"); err != nil {
		return err
	}
	_, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	tags, err := api.tags(ctx)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		fmt.Fprintln(c.stdout, msg("tags_empty"))
		return nil
	}
	table := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, msg("tags_header"))
	for _, tag := range tags {
		fmt.Fprintf(table, "%s\t%d\n", clean(tag.Name), tag.Count)
	}
	return table.Flush()
}
