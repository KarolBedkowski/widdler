widdler-ex v1.x
===============

widdler is a single binary that serves up [TiddlyWiki]s.

It can be used to serve existing wikis, or to create new ones.

Widdler-ex is fork of [Widdler](suah.dev/widdler).

Features
========

 -  TiddlyWikis are served over WebDav so you can save directly from the browser.
 -  Automatically create new wiki files by browsing to a non-existent html file.
 -  Built in .htpasswd management: add user, set password.
 -  Password protection via HTTP Basic Authentication.
 -  Multiple users (each user defined in .htpasswd has their own namespace).
 -  Optional TLS support.
 -  Backups on file save using local files or SQLite database.

Installation
============

~~~~
go install .
~~~~

Running
=======

Run `widdler-ex -h` to see all options.

[TiddlyWiki]: https://tiddlywiki.com


Single user mode
----------------

~~~~
mkdir wiki
widdler-ex serve
~~~~


Multiuser mode
--------------

~~~~
# Generate a .htpasswd file:
widdler-ex gen-htpass
Username: qbit
Passwd: ******
# Start the server
widdler-ex serve --auth=basic
~~~~

Now open your browser to <http://localhost:8080>.

Creating a new TiddlyWiki
=========================

Simply browse to the file name you wish to create. widdler will automatically
create the wiki file by downloading empty wiki from
https://tiddlywiki.com/empty.html.

Saving changes
==============

Simply hit the save button!

Backups
=======

Widdler can backup the current file before writing changes.

Widdler supports two main modes for backup (selected by `--backup.mode`
argument):

 -  copy files into a backup directory (`file` mode)
 -  store backups (full and incremental) in sqlite database (`sqlite` mode)

The `--backup.interval` argument sets the minimum time (in seconds) between write
changes for each file.


"File" mode
-----------

Additional parameters:

 -  `--backup.dir` - the directory where store backup files (in user's home directory
    in multi-user mode)
 -  `--backup.compress` - enables file compression
 -  `--backup.policy` - sets number of backups to keep, in form of
    `<number of daily backups>,<number of regular backups>`

In multi-user mode, each user has their own "backup" directory in their home.

Old backup files are deleted in background.

### Example

~~~~
widdler-ex serve --backup=file --backup.policy=7,5 --wikis ./wiki/ --backup.interval 5
~~~~


"Sqlite" mode
-------------

Additional parameters:

 -  `--backup.sqlite_file` - specifies the file name for SQLite database; Widdler will create it if it not exists.
    All users' backups are stored in one file.
 -  `--backup.policy` - sets the number of backups to keep, in form
    `<number of full backups>,<number of incremental backups>,<interval between full backups>`.
    Each part can be empty. Interval must be defined as "300s", "2h", "2h45m" etc. The default value is `8h`.
 -  `--backup.compress` - enables compression of full and big incremental backups.

Incremental backups store only the changes from last "full" backup, which can save a lot of space.
Full backups can be compressed.

For manage backups in SQLite database Widdler provide two commands:

 -  `list-backups` - lists backups for one or all users
 -  `restore-backup` - restores one backup and print it on standard output.

Old backup files are deleted in background.

### Example

~~~~
widdler-ex serve --backup=sqlite --backup.policy=7,5 --wikis=./wiki/ --backup.interval=5 --backup.sqlite_file=backup.sqlite
~~~~
