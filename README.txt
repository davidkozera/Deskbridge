DESKBRIDGE
Share a signed-in Windows desktop (screen, mouse, keyboard, sound) with
another PC on the same network. No Windows account password is involved.

FILES
  deskbridge-server.exe   For the PC you want to control (Windows on Arm).
  Install Deskbridge service.cmd / Uninstall Deskbridge service.cmd
                          Double-click on the Arm PC to add or remove the
                          startup service.
  deskbridge-client.exe   For the PC you sit at (Windows x64).
  deskbridge-source.zip   Source code (Go, standard library only).

SET UP (RUNS AT STARTUP, WORKS AT THE LOCK SCREEN)
1. On the Arm PC, put deskbridge-server.exe and the two .cmd files in
   the same folder and double-click "Install Deskbridge service.cmd".
   - Windows SmartScreen may warn about an unknown publisher: choose
     "More info" > "Run anyway".
   - Approve the administrator prompt.
   - The window shows the computer name, IP address and password.
   The installer copies the server to C:\Program Files\Deskbridge,
   registers a Windows service that starts with the PC, and allows it
   through Windows Firewall on private networks. If you were already
   using the server from this folder, your existing password is kept.
   Close any server window that is still open before installing.
2. On the x64 PC, run deskbridge-client.exe, fill in the computer name
   (or IP address) and password in the left pane, and click Connect.

To remove it, double-click "Uninstall Deskbridge service.cmd".
To change the password, edit deskbridge-password.txt in
C:\Program Files\Deskbridge as an administrator, then restart the PC.

WITHOUT INSTALLING
Double-click deskbridge-server.exe instead. It shares the desktop until
you close its window, but cannot show the lock screen or admin approval
prompts.

USING THE CLIENT
  The left pane holds every setting; the remote screen is on the right.
  The narrow strip between them hides or shows the pane.

  The pane starts hidden. If a computer name and remembered password are
  saved, the client connects by itself on launch. The pane opens on its
  own when details are missing, a connection fails, or you disconnect.

  Computer / Port / Password   Where to connect. Tick "Remember
                               password" to keep it (encrypted for your
                               Windows account) for next time.
  Frames per second            Lower it on a slow network.
  Picture quality              Affects photos and video; text is always
                               sent pixel-perfect.
  Play sound from the remote PC
  Let the remote PC go to sleep
                               Off: the remote PC stays awake so you can
                               always reach it. On: it sleeps on its
                               normal schedule, and you cannot wake it
                               from here. Stays as set until that PC
                               restarts.
  Share copy and paste         See COPY AND PASTE below.
  Full screen                  Also Ctrl+Alt+Enter, which is the way
                               back out.

  The options below Connect apply immediately, while connected.

  Click the remote screen to type on the remote PC: every key, including
  the Windows key and Alt+Tab, then goes there. Click in the pane or on
  another window to get your own keyboard back.

  Keys also stay on this PC while the pointer is on the title bar or the
  window is being dragged, so Shift-drag in PowerToys FancyZones and
  Win+arrow snapping work on the client window itself.

  To connect straight away from a shortcut:
      deskbridge-client.exe ARM-PC-NAME PASSWORD

  Signing in remotely: click or press a key to raise the lock screen,
  then type your Windows Hello PIN. Face and fingerprint sign-in need
  you to be at the PC.

COPY AND PASTE
  While connected, whatever you copy on one PC can be pasted on the
  other: text, formatted text, pictures and screenshots, spreadsheet
  cells, and files or folders copied in File Explorer.

  Files work the way they do in Remote Desktop: copying sends nothing.
  When you paste in File Explorer, Windows shows its usual copy progress
  window (with Cancel) and the files come across then. There is no size
  limit; one copy can hold up to 20000 items.

  - Paste files while still connected, and before copying something
    else on the PC they came from; otherwise the paste fails.
  - Files can be pasted into File Explorer and other places that accept
    dragged-in attachments, but not into programs that only understand
    ordinary file paths (a command prompt, some upload boxes).
  - Text, pictures and other content are sent as soon as they are
    copied, up to about 320 MB, so pasting them is instant.
  - Not carried across: content that only makes sense on the PC it was
    copied on, such as live links to another program's document.
  - On the Arm PC, copy and paste works while someone is signed in (not
    from the sign-in screen).
  - Untick "Share copy and paste" in the client to switch it off.

LIMITS
  - Ctrl+Alt+Del cannot be sent to the remote PC.
  - The PC must be awake. The service stops it idling to sleep, but
    closing the lid or pressing the power button still puts it to sleep
    unless you change that in Windows power settings.
  - Only the primary monitor is shared.
  - Sound keeps playing on the Arm PC's own speakers too.
  - One client at a time. A new connection replaces the old one.

TROUBLESHOOTING
  The service writes deskbridge.log in C:\Program Files\Deskbridge.

SERVER OPTIONS (when run without installing; all optional.
The client's settings override fps, quality, audio and sleep.)
  -port 47600      TCP port
  -password X      use this password instead of the saved one
  -fps 30          maximum frames per second
  -quality 85      JPEG quality for photo and video areas
  -noaudio         do not send sound
  -allowsleep      do not keep the PC awake

SECURITY
  The connection is encrypted (TLS 1.3) and both ends prove they know
  the password. The installed service runs with full system rights, so
  anyone on your network with the password can control the PC,
  including approving admin prompts. Treat the password accordingly and
  do not forward the port to the internet. Only administrators can read
  the installed password file.

REBUILDING
  Install Go 1.24 or later, unzip the source, then:
    set CGO_ENABLED=0
    set GOOS=windows
    set GOARCH=arm64
    go build -o deskbridge-server.exe ./cmd/deskbridge-server
    set GOARCH=amd64
    go build -o deskbridge-client.exe ./cmd/deskbridge-client
