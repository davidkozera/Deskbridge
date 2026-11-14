DESKBRIDGE
Share a signed-in Windows desktop (screen, mouse, keyboard, sound) with
another PC on the same network. No Windows account password is involved.

INSTALLING
Run Deskbridge-Setup.exe on each PC. The same file works on both; it
carries the programs for Arm and x64 PCs and installs the right ones.

  - Windows SmartScreen may warn about an unknown publisher (the setup
    program is not code-signed): choose "More info" > "Run anyway".
  - Approve the administrator prompt.
  - Tick what this PC should be:
      Server   the PC you want to control. Setup installs a Windows
               service that starts with the PC and works at the lock
               screen, opens Windows Firewall for it on private
               networks, and then shows the computer name, IP address
               and password to connect with.
      Client   the PC you sit at. Setup adds Deskbridge to the Start
               menu.
    A PC can be both.
  - Click Install.

Everything goes in C:\Program Files\Deskbridge.

To update, use the Update button (see UPDATING below), or run a newer
Deskbridge-Setup.exe and click Install again. Either way the password
and the client's settings are kept.

To remove it, use Settings > Apps > Installed apps > Deskbridge, or run
setup again and click Uninstall.

To change the password, edit deskbridge-password.txt in
C:\Program Files\Deskbridge as an administrator, then restart the PC.

The version number is shown in setup's title and in each program's
Properties > Details. The bottom of the client's left pane shows the
client's version, the version of the server it is connected to, and
how many frames a second are arriving.

UPDATING
  New versions are published at
  https://github.com/davidkozera/Deskbridge/releases

  The client checks there when it starts. When a newer version exists,
  or the remote PC runs an older version than this one, an Update
  button appears in the left pane and the window title says an update
  is available. One click then:

    1. downloads the new setup program and checks its signature;
    2. if you are connected, sends it to the remote PC, which checks
       the signature itself and installs it (the connection drops for
       a few seconds while its service restarts);
    3. updates this PC. Windows asks for administrator approval once;
       the client then closes and reopens by itself.

  Connect first if you want both PCs updated in one go. If you were not
  connected, the button comes back the next time you connect, to bring
  the remote PC up to date.

  - Updates are only accepted if they carry the publisher's signature,
    so a tampered or unofficial setup program is refused on both PCs.
  - A server that was started by hand (not installed) cannot update
    itself; run setup on that PC instead.
  - What an update did is recorded in update.log in
    C:\Program Files\Deskbridge.

USING THE CLIENT
  The left pane holds every setting; the remote screen is on the right.
  The narrow strip between them hides or shows the pane.

  The pane starts hidden. If a computer name and remembered password are
  saved, the client connects by itself on launch. The pane opens on its
  own when details are missing, a connection fails, or you disconnect.

  If a connection is lost (a network blip, the remote PC restarting,
  this PC waking from sleep), the client reconnects by itself: it tries
  after 1, 2 and 4 seconds and then every 8, for as long as it takes,
  and returns to full screen if that is where you were. The same goes
  for the saved computer at launch if it is not answering yet. To give
  up, open the pane and click "Stop trying". It does not reconnect after
  you click Disconnect, if the password is refused, or when another PC
  connects to the same server and takes over.

  A lost connection is noticed within about eight seconds even when
  nothing is happening on screen, because the two PCs check on each
  other every couple of seconds.

  Computer / Port / Password   Where to connect. The Computer box is
                               also a list: every computer you connect
                               to is saved in it, most recent first,
                               and picking one fills in its port and
                               password. Tick "Remember password" to
                               keep that computer's password (encrypted
                               for your Windows account).
  Forget this computer         Removes the computer shown, and its
                               saved password, from the list.
  Remote display               Which of the remote PC's displays to
                               show, or all of them side by side. The
                               choice is remembered. The list follows
                               displays being plugged in or removed.
                               While the lock screen or an admin prompt
                               is up, all displays are shown, because
                               those can appear on any of them.
  Frames per second            Up to 120. This is a ceiling, not a
                               promise: the rate actually arriving is
                               shown at the bottom of the pane. Lower it
                               on a slow network. Above 60 needs the
                               remote PC's display set to a refresh rate
                               that high, and a screen on this PC that
                               can show it.
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
  Use H.265 video for motion   See VIDEO FOR MOTION below.
  Fast screen capture          The Arm PC takes frames straight from its
                               graphics card, and only when the screen
                               changes: smoother, and far less work when
                               idle. The pane shows "Screen capture:
                               fast" when it is in use. If the picture
                               misbehaves, untick it to go back to the
                               standard method; Deskbridge also falls
                               back by itself if fast capture fails.
  Full screen                  Also Ctrl+Alt+Enter, which is the way
                               back out.
  Send to remote PC            A menu of things that cannot be typed
                               across: Ctrl+Alt+Del (also Ctrl+Alt+End
                               on the keyboard), Lock, Task Manager,
                               Sign out, Restart and Shut down. The
                               last three ask first. After Restart the
                               remote PC comes back by itself; after
                               Shut down someone has to turn it on.

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

VIDEO FOR MOTION
  A still screen is sent as exact pixels, so text stays perfectly sharp.
  When a large part of the screen keeps changing for more than about
  half a second (a film, scrolling, dragging a window), the server
  switches those areas to H.265 video, using the Arm PC's hardware
  encoder. About a third of a second after the motion stops it switches
  back and redraws those areas crisply. The rest of the screen stays
  pixel-perfect throughout. Short bursts, such as a window opening or
  closing, are always sent the standard way.

  - The client needs an H.265 decoder: install "HEVC Video Extensions"
    from the Microsoft Store if it is missing. Without it, H.264 (built
    into Windows) is used instead, and the client's pane says so.
  - Decoding is done by the client PC's graphics card when it can
    decode the codec, and in software otherwise (or if the card's
    decoder fails; Deskbridge then switches over by itself).
  - The pane shows which encoder and decoder are in use the first time
    video starts. You want "hardware" for the encoder and "on the
    graphics card" for the decoder. If the encoder says otherwise and
    the Arm PC runs hot, untick the option.
  - "Picture quality" sets the video bit rate; "Frames per second" its
    frame rate.
  - If anything goes wrong with the encoder or decoder, Deskbridge falls
    back to the standard method by itself. Untick "Use H.265 video for
    motion" to switch it off altogether.
  - Details of what the server tried are in deskbridge.log.

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
  - Ctrl+Alt+Del needs the installed service on the remote PC; a server
    started by hand cannot send it.
  - The PC must be awake. The service stops it idling to sleep, but
    closing the lid or pressing the power button still puts it to sleep
    unless you change that in Windows power settings.
  - "All displays" shows the displays as Windows arranges them, with
    black where none reaches. It is a large picture: expect it to be
    slower than a single display, and H.264 video may not be available
    for it.
  - Sound keeps playing on the Arm PC's own speakers too.
  - One client at a time. A new connection replaces the old one.

TROUBLESHOOTING
  The service writes deskbridge.log in C:\Program Files\Deskbridge.

RUNNING THE SERVER WITHOUT INSTALLING
  deskbridge-server.exe can also be started by hand from a command
  prompt. It then shares the desktop until its window is closed, but
  cannot show the lock screen or admin approval prompts. Options (the
  client's settings override fps, quality, audio and sleep):
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

REBUILDING AND PUBLISHING
  The source is Go, standard library only. Install Go 1.24 or later and
  run build.sh from the source folder (on Windows, in Git Bash). It
  builds the server and client for both processor types, stamps them
  with the icon and the version in the VERSION file, and packs them
  into dist/Deskbridge-Setup-<version>.exe.

  Publishing is automatic: when the VERSION file changes on the main
  branch, GitHub builds the setup program, signs it and publishes it as
  a release (.github/workflows/release.yml). Signing uses the private
  key stored as the repository secret UPDATE_SIGNING_KEY; its public
  half is in internal/update/key.go. A setup program built by hand is
  not signed, so it installs normally but is not offered as an update.
