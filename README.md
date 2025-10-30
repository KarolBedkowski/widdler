widdler
=======

widdler is a single binary that serves up
[TiddlyWiki](https://tiddlywiki.com)s.

It can be used to serve existing wikis, or to create new ones.

# Features

- TiddlyWikis are served over WebDav so you can save directly from the browser.
- Automatically create new wiki files by browsing to a non-existent html file.
- Built in .htpasswd management (Adding users).
- Password protection via HTTP Basic Authentication.
- Multiple users (adding another user to the .htaccess file creates a new user
  namespace).
- Optional TLS support.
- Backups on file save.

# Installation

```
go install .
```

By default for git backups is used external (system) git. To use buildin implementation compile/install with
`-tags gitint` parameter. Builtin implementation may be faster on windows system.


# Running

Run `widdler -h` to see all options.

## Single user mode
```
mkdir wiki
widdler
```

## Multiuser mode

```
mkdir wiki
cd wiki
# Generate a .htpasswd file:
widdler -gen
Username: qbit
Passwd: ******
# Start the server
widdler -auth=basic
```

Now open your browser to [http://localhost:8080](http://localhost:8080).

# Creating a new TiddlyWiki

Simply browse to the file name you wish to create. widdler will automatically
create the wiki file by downloading empty wiki from https://tiddlywiki.com/empty.html.


# Saving changes

Simply hit the save button!


# Backups

Widdler can backup current file before write changes.

Widdler support two main modes for backup (selected by `-backup.mode` argument):
- copy files into backup directory ("file" mode)
- put file into git repository ("git", "git-once" modes)

`-backup.interval` argument set minimal time (in seconds) between write changes of each file.

## "File" mode

"file" mode use additional parameters:

* `-backup.dir` - directory for backup files (directory in user home in multi-user mode)
* `-backup.compress` - enable file compression
* `-backup.keep_daily` - limit number of backup files to keep; one file per day
* `-backup.keep_on_write` - limit number of backup files created today

Set `keep_daily` or `keep_on_write` if no `mode` is given enable "file" mode.

Old backup files are deleted in background.

Example:

```
widdler -backup.keep_daily 7 -backup.keep_on_write 5 -wikis ./wiki/ -backup.interval 5
```

## "Git*" modes

"git" and "git-once" open or create if not exists git repository in wikis directory (or user home).

"git" mode commit each change of file (if file change and after `interval` since last file write).

"git-once" create only one backup on first file change each day (or since widdler start).

Example:

```
widdler -wikis ./wiki/ -backup.mode git-once
widdler -wikis ./wiki/ -backup.interval 10 -backup.mode git
```
