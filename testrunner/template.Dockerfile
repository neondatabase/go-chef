# Why build go-chef inside the container? It should have better cross-platform support.
#
# Docker on Mac is internally just Linux, so using a binary compiled on the host might actually
# *not* work if we just copied it in here. So instead we build go-chef in the same environment that
# we use it.
FROM {{ .GoBaseImage }} AS chef-builder
COPY main.go go.mod go.sum /workspace/
RUN cd /workspace && go build -o /go-chef main.go

# Create a "base" image containing just go-chef, so it doesn't have anything else cached.
FROM {{ .GoBaseImage }} AS chef
COPY --from=chef-builder /go-chef /usr/bin/go-chef
WORKDIR /workspace

# Clone the source that we'd like
FROM {{ .GitBaseImage }} AS src
RUN {{ .InstallGit }}
# these two steps should be separate, because git installation is *much* less likely to change
RUN git clone --branch={{ .GitBranch }} {{ .GitURL }} /gitrepo

FROM chef AS planner
COPY --from=src /gitrepo .
RUN go-chef --prepare recipe.json

# pre-define this so it's referenced by the individual build targets
FROM chef AS cooked
COPY --from=planner /workspace/recipe.json recipe.json
RUN go-chef --cook recipe.json
COPY --from=src /gitrepo .

# Build each target
{{range .Targets}}
FROM cooked AS builder-{{.StageSuffix}}
RUN go build -o /dev/null ./{{.MainPath}} && touch success
{{end}}

# Force that each target is actually built
FROM {{ .GitBaseImage }}
{{range .Targets}}
COPY --from=builder-{{.StageSuffix}} /workspace/success success.{{.StageSuffix}}
{{end}}
# clean up, so the image is minimally different.
RUN rm success.*
