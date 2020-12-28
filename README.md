
# jsupdate

*jsupdate* is a tool for keeping your `package.json` file updated without breaking
everything.

For projects of sufficient size, just running `yarn upgrade` will break your code. `jsupdater` can figure out which updates are safe to apply, and which are not.

## Usage

```
$ go get github.com/crewjam/jsupdate
$ jsupdate
```

## Command Line Options

* `-c` - The root directory of the module to update (default ".")
* `-test` - The command that evaluates if an update works (default "go test ./...")

## How it works

First, we run `npm outdated` to determine which packages are to be upgraded. Then we check that candidate set of upgrades to see if the tests pass. If the tests do not pass, we split the upgrades in half (bisect them), and check
  each half separately. We repeat this process until we have a list of good and bad updates. Finally, we reset `package.json` to that it contains only the good updates.
