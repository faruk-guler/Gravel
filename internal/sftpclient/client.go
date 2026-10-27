package sftpclient

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Entry is a normalized directory/file listing for the UI file manager.
type Entry struct {
	Name    string
	Path    string
	IsDir   bool
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
	Owner   string
	Group   string
}

// Client wraps an SFTP session that shares an established SSH connection.
type Client struct {
	sftp *sftp.Client
	ids  *idMaps
}

// New opens an SFTP subsystem over the given SSH client.
func New(sshClient *ssh.Client) (*Client, error) {
	if sshClient == nil {
		return nil, fmt.Errorf("sftpclient: nil ssh client")
	}
	sc, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftpclient: new sftp client: %w", err)
	}
	c := &Client{sftp: sc}
	c.ids = &idMaps{c: c} // back-pointer so the maps can read over this session
	return c, nil
}

// Close terminates the SFTP session (the underlying SSH client is unaffected).
func (c *Client) Close() error {
	if c == nil || c.sftp == nil {
		return nil
	}
	return c.sftp.Close()
}

// Raw returns the underlying *sftp.Client so callers (e.g. the transfer queue)
// can drive file operations directly.
func (c *Client) Raw() *sftp.Client { return c.sftp }

// Home returns the remote user's home directory. A freshly opened SFTP session
// starts in the authenticated user's home, so its working directory is exactly
// what a bare "cd" in the shell lands on. Falls back to "/" when the server
// does not answer, which keeps path resolution usable rather than empty.
func (c *Client) Home() string {
	wd, err := c.sftp.Getwd()
	if err != nil || wd == "" {
		return "/"
	}
	return wd
}

// joinRemote builds a POSIX-style remote path independent of the host OS.
func joinRemote(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return path.Join(dir, name)
}

// toEntry converts an os.FileInfo returned by the SFTP server, resolving the
// numeric owner/group carried in the raw attributes when they are available.
func (c *Client) toEntry(fi os.FileInfo, dir string) Entry {
	e := Entry{
		Name:    fi.Name(),
		Path:    joinRemote(dir, fi.Name()),
		IsDir:   fi.IsDir(),
		Size:    fi.Size(),
		Mode:    fi.Mode(),
		ModTime: fi.ModTime(),
	}
	if st, ok := fi.Sys().(*sftp.FileStat); ok && st != nil {
		e.Owner = c.ids.user(st.UID)
		e.Group = c.ids.group(st.GID)
	}
	return e
}

// List returns the entries of dir sorted with directories first, then by name.
func (c *Client) List(dir string) ([]Entry, error) {
	if dir == "" {
		dir = "/"
	}
	fis, err := c.sftp.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("sftpclient: list %s: %w", dir, err)
	}
	entries := make([]Entry, 0, len(fis))
	for _, fi := range fis {
		if fi == nil {
			continue
		}
		entries = append(entries, c.toEntry(fi, dir))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// Walk returns the path of every non-directory entry under dir, depth first, in
// sorted order. Symlinks are collected as plain files rather than descended
// into, so a link cycle cannot make the walk loop.
//
// limit bounds the result: "download folder" on something like / would
// otherwise enqueue an unbounded number of transfers.
func (c *Client) Walk(dir string, limit int) ([]string, error) {
	var out []string
	var walk func(string) error
	walk = func(d string) error {
		entries, err := c.List(d)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if len(out) >= limit {
				return fmt.Errorf("sftpclient: walk %s: more than %d files", dir, limit)
			}
			if e.IsDir {
				if err := walk(e.Path); err != nil {
					return err
				}
				continue
			}
			out = append(out, e.Path)
		}
		return nil
	}
	if err := walk(dir); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Stat returns metadata for a single remote path.
func (c *Client) Stat(p string) (Entry, error) {
	fi, err := c.sftp.Stat(p)
	if err != nil {
		return Entry{}, fmt.Errorf("sftpclient: stat %s: %w", p, err)
	}
	dir := path.Dir(p)
	e := c.toEntry(fi, dir)
	e.Path = p
	if e.Name == "." || e.Name == "/" {
		e.Name = fi.Name()
	}
	return e, nil
}

// Mkdir creates a single remote directory.
func (c *Client) Mkdir(p string) error {
	if err := c.sftp.Mkdir(p); err != nil {
		return fmt.Errorf("sftpclient: mkdir %s: %w", p, err)
	}
	return nil
}

// Touch creates an empty remote file with the server's default permissions. It
// refuses to touch a path that already exists, so the UI's "New file" action can
// never truncate something the user still needs (pkg/sftp's OpenFile takes no
// mode argument, unlike Create, which would truncate).
func (c *Client) Touch(p string) error {
	f, err := c.sftp.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return fmt.Errorf("sftpclient: create %s: %w", p, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("sftpclient: close %s after create: %w", p, err)
	}
	return nil
}

// Rename moves/renames a remote path.
func (c *Client) Rename(oldPath, newPath string) error {
	if err := c.sftp.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("sftpclient: rename %s -> %s: %w", oldPath, newPath, err)
	}
	return nil
}

// Remove deletes a file, or a directory recursively.
func (c *Client) Remove(p string) error {
	fi, err := c.sftp.Stat(p)
	if err != nil {
		return fmt.Errorf("sftpclient: stat %s: %w", p, err)
	}
	if !fi.IsDir() {
		if err := c.sftp.Remove(p); err != nil {
			return fmt.Errorf("sftpclient: remove %s: %w", p, err)
		}
		return nil
	}
	return c.removeDirRecursive(p)
}

func (c *Client) removeDirRecursive(dir string) error {
	walk := c.sftp.Walk(dir)
	var paths []string
	for walk.Step() {
		if walk.Err() != nil {
			return walk.Err()
		}
		p := walk.Path()
		if p == dir {
			continue
		}
		paths = append(paths, p)
	}
	// Remove deepest entries first, then the root directory itself.
	for i := len(paths) - 1; i >= 0; i-- {
		if err := c.sftp.Remove(paths[i]); err != nil {
			return fmt.Errorf("sftpclient: remove %s: %w", paths[i], err)
		}
	}
	return c.sftp.RemoveDirectory(dir)
}

// Download writes the remote file to a local path, reporting progress if non-nil.
// A failed copy removes the local file: a truncated download left behind would
// look like a finished one to the user and to any later open of that path.
func (c *Client) Download(remotePath, localPath string, progress func(done, total int64)) error {
	src, err := c.sftp.Open(remotePath)
	if err != nil {
		return fmt.Errorf("sftpclient: open remote %s: %w", remotePath, err)
	}
	defer src.Close()

	var total int64 = -1
	if fi, err := src.Stat(); err == nil {
		total = fi.Size()
	}

	dst, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("sftpclient: create local %s: %w", localPath, err)
	}

	err = copyWithProgress(dst, src, total, progress)
	// Close before the removal: Windows keeps an open file undeletable, and a
	// write error only surfaces once the handle is flushed shut.
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(localPath)
	}
	return err
}

// Upload copies a local file to the remote path, reporting progress if non-nil.
func (c *Client) Upload(localPath, remotePath string, progress func(done, total int64)) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("sftpclient: open local %s: %w", localPath, err)
	}
	defer src.Close()
	var total int64 = -1
	if fi, err := src.Stat(); err == nil {
		total = fi.Size()
	}
	return c.UploadReader(remotePath, src, total, progress)
}

// UploadReader streams content from r into a remote file. total may be -1 if
// unknown; progress is invoked with bytes written so far.
func (c *Client) UploadReader(remotePath string, r io.Reader, total int64, progress func(done, total int64)) error {
	dst, err := c.sftp.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sftpclient: create remote %s: %w", remotePath, err)
	}
	defer dst.Close()
	return copyWithProgress(dst, r, total, progress)
}

// copyWithProgress streams from src to dst in chunks, invoking progress optionally.
func copyWithProgress(dst io.Writer, src io.Reader, total int64, progress func(done, total int64)) error {
	buf := make([]byte, 32*1024)
	var done int64
	for {
		nr, err := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			if nw > 0 {
				done += int64(nw)
				if progress != nil {
					progress(done, total)
				}
			}
			if werr != nil {
				return werr
			}
			if nr != nw {
				return io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
