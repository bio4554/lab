// Package stacks embeds the Dockerfile templates for agent images so
// labd can build them without depending on the repo checkout at run
// time. internal/labd/runtime is the only intended consumer.
package stacks

import "embed"

// FS holds one directory per stack, each containing a Dockerfile.
// "base" is the shared bottom layer every other stack builds FROM.
//
//go:embed */Dockerfile
var FS embed.FS
