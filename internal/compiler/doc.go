// Package compiler reads a GitHub Actions workflow and event, expands reusable
// workflows and matrices, resolves action source, and produces one job plan per
// job plus the Buildkite pipeline that runs them.
package compiler
