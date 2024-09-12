# go-chef

`go-chef` is a tool to cache compiled dependencies when `docker build`ing Go programs.

*Inspired by [cargo-chef](https://github.com/LukeMathWalker/cargo-chef).*

**Table of Contents**

1. [Usage](#usage)
2. [How it works](#how-it-works)
3. [When to use it?](#when-to-use-it)

## Usage

In short:

```dockerfile
FROM golang:$TAG AS chef
RUN go install github.com/neondatabase/go-chef@v0.1.0
WORKDIR /workspace

# Read the dependencies information to produce a JSON file representing what packages need to be
# compiled.
#
# This is NOT cached, but if the "recipe" (describing which packages are used) is the same, then the
# next step will be.
FROM chef AS planner
COPY . .
RUN go-chef --prepare recipe.json

# Build dependencies using the recipe.
# THIS IS CACHED (usually).
FROM chef as builder
COPY --from=planner /workspace/recipe.json recipe.json
# If you're building with e.g. CGO_ENABLED=0, you should pass that here as well, otherwise the
# pre-built dependencies won't be reused later.
RUN go-chef --cook recipe.json

# Build the actual binary (NOT CACHED, but we use the dependencies that are)
COPY . .
RUN go build path/to/main.go # your code here!
```

## How it works

When you run `go-chef --prepare recipe.json`, `go-chef` reads your source tree to discover all
packages referenced in import statements, under various compilation conditions (e.g., "only on
Linux" or "only when certain build tags are set").

The finalized `recipe.json` contains your `go.mod`, `go.sum`, and this sorted list of packages,
meaning that it's exactly equal across source code changes as the set of packages imported has not
changed.

Then, when you `go-chef --cook recipe.json`, we create and `go build` a small `main.go` that just
imports all the packages used (in addition to auxiliary files for each set of compilation
conditions). Because the `recipe.json` rarely changes, this docker layer is usually cached.

And finally, after you copy the rest of the source in, running `go build` uses the go cache from the
previous `go-chef --cook` so that you're only recompiling the local code.

## When to use it?

Generally, `go-chef` provides meaningful speed-ups when either:

1. You have multiple go programs in the same repo, with overlapping dependencies
2. You have very large dependencies

If neither of these are true, it's probably not worthwhile!
