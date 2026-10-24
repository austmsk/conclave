# Sandbox image for Java 21 repositories built with plain javac (ETS in
# Milestone 1). Nothing in it names a repository: the toolchain commands
# come from deploy/local/toolchains.yaml, and the working tree is streamed
# in by the provider.
#
# What internal/sandbox/docker needs from an image, and where this gets it:
#   /bin/sh             the provider's entrypoint and init loop, and every
#                       file script (dash, from the base)
#   tar                 the working tree is unpacked with tar -x
#   cat wc mkdir stat   ReadFile and WriteFile (stat -c %h)
#   find grep           the search tool (find ... -prune ... -exec grep -snHE)
#                       Busybox and GNU both qualify; the provider's tests
#                       run on busybox (alpine/git), this image on GNU
#   sleep kill          the init loop that kills strays after each command
#   git                 Diff, and apply_patch's check and apply
#   PATH, JAVA_HOME     from the base image's ENV: the provider sets only
#                       HOME, TMPDIR, CI and USER (FR-SEC23)
#
# The provider runs the container as uid 1000, read-only, with every
# capability dropped and no-new-privileges, and mounts /workspace,
# /home/agent and /tmp as tmpfs. So nothing here has to be writable, and
# the image must declare no VOLUME: Create refuses a container that ends up
# with a mount.
#
# The base is pinned by its multi-platform index digest (FR-SEC30). The git
# package is not pinned to a version, because Ubuntu drops old versions
# from its archive and a pinned build would stop working. What the worker
# runs is the built image, pinned by its own digest, so a rebuild that
# picks up a newer git is a new digest the owner reviews.
#
# Build:
#   docker build -t conclave-sandbox-java21 deploy/images/java21
#   docker inspect --format '{{index .RepoDigests 0}}' <pushed image>

# eclipse-temurin:21-jdk-noble, JDK 21.0.12.1+1 on Ubuntu 24.04.
FROM docker.io/library/eclipse-temurin:21-jdk-noble@sha256:107215954359f1ab0be058a247d7c64ed6e69c71c1682d471d38ef751ec8e964

# git for Diff and apply_patch. Then strip setuid and setgid bits: the
# provider's no-new-privileges already neutralizes them, but there is no
# reason to ship any.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git \
 && rm -rf /var/lib/apt/lists/* \
 && find / -xdev -perm /6000 -type f -exec chmod a-s {} +

# The provider sets its own entrypoint. Clearing the base image's
# certificate hook means nothing runs it when the image is used directly.
ENTRYPOINT []

# The provider sets the user; this keeps the image non-root when run any
# other way. Ubuntu 24.04 already has uid 1000 (ubuntu).
USER 1000:1000
WORKDIR /workspace
