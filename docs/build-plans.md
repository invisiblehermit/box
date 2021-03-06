Build Plans power Box and are the core of its functionality. This
document describes how you can power box by writing build plans, and
what techniques to use while writing them.

## Box Build Plans are Programs

Exploit this! Use functions! Save variables and constants!

run this plan with: `GOLANG_VERSION=1.7.5 box <plan file>`

```ruby
from "ubuntu"

# this function will create a new layer running the command inside the
# function, installing the required package.
def install_package(pkg)
  run "apt-get install '#{pkg}' -y"
end

run "apt-get update"
install_package "curl" # `run "apt-get install curl -y"`

# get the local environment's setting for GOLANG_VERSION, and set it here:
go_version = getenv("GOLANG_VERSION")
run %Q[curl -sSL \
    https://storage.googleapis.com/golang/go#{go_version}.linux-amd64.tar.gz \
    | tar -xvz -C /usr/local]
```

## Powered by mruby

Box uses the [mruby programming language](https://mruby.org/). It does
this to get a solid language syntax, functions, variables and more.
However, it is not a fully featured Ruby such as MRI and contains almost
zero standard library functionality, allowing for only the basic types,
and no I/O operations outside of the box DSL are permitted.

You can however:

* Define classes, functions, variables and constants
* Access the environment through the
  [getenv](/user-guide/functions/#getenv) box function (which is also
  [omittable](/user-guide/cli/#-omit-o) if you don't want people to use
  it)
* Retrieve the contents of container files with [read](/user-guide/functions/#read)
* [import](/user-guide/functions/#import) libraries (also written in
  mruby) to re-use common build plan components.

## Tagging and Image Editing

You can tag images mid-plan to create multiple images, each subsets (or
supersets, depending on how you look at it) of each other.

Additionally, you can use functions like
[after](/user-guide/verbs/#after), [skip](/user-guide/functions/#skip),
and [flatten](/user-guide/verbs/#flatten) to manipulate images in ways
you may not have considered:

```ruby
from :ubuntu
skip do
  run "apt-get update"
  run "apt-get install curl -y"
  run "curl -sSL -O https://github.com/box-builder/box/releases/download/v0.4.2/box_0.4.2_amd64.deb"
  tag :downloaded
end

run "dpkg -i box*.deb"
after do
  flatten
  tag :installed
end
```

Because the tag is processed after the final edits occur, tag
`installed` will not contain the update manifests, `curl`, or the
`.deb`. Tag `downloaded` will, though (but will also not contain the
installed package).

## The Debugger

[debug](/user-guide/verbs/#debug) is a really powerful tool for
establishing a breakpoint in your build plan where you can investigate
your container.

Just add a line:

```ruby
debug
```

And as soon as it is reached you will see a shell appear in your
console.

## Verb Properties

[copy](/user-guide/verbs/#copy), [run](/user-guide/verbs/#run), and
other support properties to modify their behavior. You should check out
what you can do in the docs, but here are a few examples.

```ruby
# `output: false` runs vim without attaching to the TTY, allowing you to run this
# command in a logging environment as well as a terminal one
run "vim +PluginInstall -c 'set nomore' +qall", output: false

# `ignore_list` takes an array of string patterns to ignore from the
# file list.
copy "*", ".", ignore_list: %w[foo bar]
```

## Experiment with the REPL

The REPL is a line interpreter that immediately gives you feedback on
any given statement, allowing you to build an image interactively.

Works great with `debug`!

Launch with `box repl`.

## Parallel Building

`box multi` can build several plans at once. Just supply multiple
filenames!

Multi-builds do not return run output and build in parallel, so there
will be a lot of output in many cases.
