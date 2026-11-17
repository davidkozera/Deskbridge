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
               screen, and shows the computer name, IP address and
               password to connect with. It also adds a Windows
               Firewall rule ("Deskbridge server") for private and
               work networks, checks that the rule is really there,
               and says so. If the PC's network is set to Public,
               setup says that too: either tick "Also allow
               connections while on a public network" (a second rule
               that lets in only PCs on the same network) or set the
               network to Private in Windows.
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

INTERNET (Alpha versions only, for now)
  Normally the client reaches a PC by its own address, which only works
  on the same network. With a relay, a saved connection also works from
  anywhere, and no port has to be opened on any router: the server and
  the client both connect out to the relay, which passes the session
  between them. It is the same encrypted, password-checked connection
  either way; the relay cannot see inside it.

  In this version the whole session travels through the relay. A later
  one will let the two PCs talk directly after being introduced.

  1. Set up the relay, once. It is a small container that runs on
     Azure Container Apps.

     The quick way: on the project's GitHub page, switch to the alpha
     branch and click the "Deploy to Azure" button. Choose a
     subscription, a resource group and a region, click Review + create
     and then Create. When it has finished, open Outputs on the
     deployment and copy relayCode.

     Or by hand: paste these commands into Azure Cloud Shell (the >_
     button at portal.azure.com, Bash):

       RG=deskbridge
       LOC=canadacentral
       KEY=$(openssl rand -hex 24)
       az provider register --namespace Microsoft.App --wait
       az group create --name $RG --location $LOC
       az containerapp env create --name deskbridge-env \
         --resource-group $RG --location $LOC --logs-destination none
       FQDN=$(az containerapp create --name deskbridge-relay \
         --resource-group $RG --environment deskbridge-env \
         --image ghcr.io/davidkozera/deskbridge-relay:alpha \
         --target-port 8080 --ingress external \
         --min-replicas 1 --max-replicas 1 --cpu 0.25 --memory 0.5Gi \
         --secrets relay-key=$KEY \
         --env-vars DESKBRIDGE_RELAY_KEY=secretref:relay-key \
         --query properties.configuration.ingress.fqdn --output tsv)
       echo "Relay code:  https://$FQDN#$KEY"

     The last line prints your relay code. Keep it somewhere safe: it
     is the only copy of the key. Opening the https address in a
     browser should show a line beginning "Deskbridge relay".

     - Exactly one copy must run (--max-replicas 1), or the two ends
       can land on different copies and never meet.
     - If a command objects to --logs-destination, leave that part out.
     - To move the relay to a newer version later:
         az containerapp update --name deskbridge-relay \
           --resource-group deskbridge \
           --image ghcr.io/davidkozera/deskbridge-relay:VERSION

  2. In the client, copy the relay code, then choose
     More > Internet access: use the relay code I copied.

  3. Connect once to each saved PC while on its network. The client
     tells it the relay's address and key, the PC connects out to the
     relay and stays connected, and the client remembers the ID it goes
     by there. That is all the setting up a server needs.

  From then on, opening a saved connection tries the PC's own address
  first. If that has not answered within a second and a half, the relay
  is tried as well, and whichever answers is used (the PC's own address
  if both do). The bottom of the pane says which: "Local network" or
  "Internet (relay)". Checking and
  updating saved servers works through the relay in the same way.

  Speed: away from home, the limit is usually the upload speed of the
  remote PC's internet connection, which is far below a local network.
  The server measures what the connection carries and fits the picture
  to it: a large change is sent in pieces, top of the screen first,
  photographs are compressed a little harder, and video for motion
  starts sooner and at a bit rate the connection can take. The bottom
  of the pane shows what was measured, for example
  "Internet (relay) | 14 Mbit/s | 85 ms". No speed is shown while the
  connection has not been seen to hold anything up, as on a local
  network, where nothing changes. "Video for motion" makes the biggest
  difference on a slow connection; keep it ticked.

  - Only saved connections are set up this way, and each client needs
    the relay code. Every client should use the same one: a server
    follows whichever relay it was told about last.
  - More > Internet access: turn off stops this client using the relay,
    and can tell the connected PCs to stop as well.
  - The relay key decides who may use the relay. The PC's own password
    is still needed to connect to it.
  - A PC that is off or asleep is not connected to the relay and cannot
    be reached, here as on a local network.
  - Networks that only allow web traffic through a proxy server are not
    supported yet.
  - A Mainline client does not keep the relay code or the IDs. After
    going back to Mainline and returning to Alpha, paste the code and
    connect to each PC on its network again.

UPDATING
  New versions are published at
  https://github.com/davidkozera/Deskbridge/releases

  The client checks there when it starts and a few times a day. It
  also asks every saved connection that has a remembered password which
  version its server runs, whether or not you are connected to it. This
  check shows and controls nothing on those PCs and does not disturb
  anyone using them. When a newer version exists, or any of those
  servers runs an older version than this PC, an Update button appears
  in the left pane, saying how many PCs it covers, and the window title
  says an update is available. One click then:

    1. downloads the new setup program and checks its signature;
    2. sends it to the connected PC and to every other saved server
       that is behind and can be reached, all at the same time. Each
       checks the signature itself and installs it (connections to it
       drop for a few seconds while its service restarts);
    3. updates this PC. Windows asks for administrator approval once;
       the client then closes and reopens by itself.

  A saved server that is off, asleep or away is skipped and picked up
  by a later check. One without a remembered password is updated the
  next time you connect to it. Servers older than 1.5.0 cannot answer
  the version check, so they are taken to need the update, and they
  receive it over an ordinary connection, which takes the place of
  anyone else connected to them at that moment.

  Release channels (More > Releases)
    Mainline   finished versions. This is what every PC follows unless
               you choose otherwise.
    Beta       test versions that are nearly ready.
    Alpha      experimental versions, where new features are tried
               first. Expect faults.

    The choice belongs to this client. After changing it, the Update
    button offers to move this PC, the PCs it is connected to and the
    other saved servers to the newest version of that channel. Nothing
    changes until you click it. Alpha also takes a Beta or Mainline
    version when that is the newest, and Beta takes a Mainline one.

    To go back, choose Releases: Mainline and click the button, which
    then reads "Switch ... to" the newest Mainline version. That is the
    only case where a PC is moved to an older version; a PC on Mainline
    is never moved back. If a remote PC can no longer be reached after
    a test version, run the Mainline setup program on it.

    Every PC must run 1.10.0 or later before it can take a Beta or
    Alpha version, so update on Mainline first. If two clients set to
    different channels use the same server, each offers to move it to
    its own channel; choose the same channel on both.

  - Updates are only accepted if they carry the publisher's signature,
    so a tampered or unofficial setup program is refused on both PCs.
  - A server that was started by hand (not installed) cannot update
    itself; run setup on that PC instead.
  - What an update did is recorded in update.log in
    C:\Program Files\Deskbridge.

USING THE CLIENT
  The left pane holds every setting; the remote screen is on the right.
  The narrow strip between them hides or shows the pane.

  The pane starts hidden. On launch the client reopens the tabs that
  were open when it was last closed (each saved connection among them
  with a remembered password) and puts the same one in front. If none
  were open it connects to nothing and the pane opens. The pane also
  opens on its own when details are missing, a connection fails, or you
  close the last connection.

  SEVERAL CONNECTIONS AT ONCE
  Each connection has a tab in the strip above the remote screen,
  labelled with its connection name. To open another, pick or type it in
  the pane and click Connect: it gets a tab of its own and the ones
  already open stay connected (up to 12).

  - Click a tab to switch to it. Ctrl+Alt+PageDown and Ctrl+Alt+PageUp
    step to the next and previous tab, which also works in full screen.
  - The strip ends with "Send" (the same menu as the pane's "Send to
    remote PC": Ctrl+Alt+Del, Lock, Task Manager and so on) and "Full
    screen", so neither needs the pane open.
  - In full screen the strip is put away. Rest the pointer at the very
    top edge of the screen, in the middle, and it drops down as a
    toolbar: the tabs, Send, and "Exit full screen". It goes away once
    the pointer has left it. It appears only over the middle of the
    screen, so the remote PC's own top corners stay within reach, and
    not for a pointer that is only passing the edge.
  - Click a tab's x, or middle-click the tab, to close that connection.
    The pane's Disconnect button closes the tab in front.
  - The tab in front is the one you see and control. It alone gets the
    keyboard and mouse, plays sound and shares copy and paste. The
    pane's Remote display and Remote resolution lists, "Send to remote
    PC" and Full screen are about it, and switching tabs puts its
    details in the pane's boxes.
  - The tabs behind stay connected at two frames a second, without
    sound or video for motion, so they cost little and their pictures
    are current when you switch to them.
  - Frames per second, picture quality and the tick boxes belong to each
    connection: the pane shows those of the tab in front, changing one
    changes it for that connection only, and each saved connection
    remembers its own. A connection saved before version 1.7.0 starts
    with whatever the pane shows when you connect.
  - A tab whose label ends in "(connecting...)" is still connecting or
    is reconnecting; "(not connected)" means the attempt failed or
    another PC took that connection over. Click Connect to try again,
    or close the tab.
  - A remote PC can be open in one tab only.

  If a connection is lost (a network blip, the remote PC restarting,
  this PC waking from sleep), its tab reconnects by itself: it tries
  after 1, 2 and 4 seconds and then every 8, for as long as it takes,
  and returns to full screen if that is where you were. The same goes
  for the saved computer at launch if it is not answering yet. To give
  up, close the tab (or click "Stop trying" in the pane). A tab does not
  reconnect if the password is refused, or if the server dropped it to
  make room for another PC (a ninth one, or any second one on a server
  older than 1.8.0).

  SEVERAL PCS CONNECTED TO THE SAME REMOTE PC
  From version 1.8.0 a server serves up to 8 PCs at once (before, a new
  connection took the place of the old one). They all see its one real
  screen; it does not give each its own desktop.

  - One PC at a time has the mouse and keyboard. A PC takes control by
    clicking, typing or scrolling on the remote screen, by dropping
    files on it, or with one of the "Send to remote PC" actions. Moving
    the pointer over the remote screen does not take control.
  - While another PC is in control you are watching: its pointer is
    drawn on the picture, the tab says "(watching)", and a "Take
    control" button sits at the top of the remote screen (in full
    screen too). Whatever the other PC was holding down is let go when
    control passes.
  - Each PC chooses which of the remote PC's displays it shows, and has
    its own frame rate, quality and sound. Every PC with sound ticked
    hears it.
  - The resolution is the remote PC's own, so it changes for everyone.
    Only the PC in control can change it, and a connection's saved
    resolution is applied when it connects only if no other PC is in
    control then.
  - Copy and paste is shared with the PC in control. Something you copy
    on your PC while watching is sent when you take control.
  - When the PC in control disconnects and one other is left, that one
    takes over; with several left, the first to act does.
  - A client older than 1.8.0 can connect alongside others and takes
    control the same way, but has no Take control button and does not
    see the other PC's pointer.

  A lost connection is noticed within about eight seconds even when
  nothing is happening on screen, because the two PCs check on each
  other every couple of seconds.

  Connection name              What you call this connection, such as
                               "Office laptop". It is also the list of
                               saved connections, most recent first:
                               picking one fills in everything else.
                               Leave it empty and the connection is
                               called by its computer. "New
                               connection..." at the end of the list
                               empties the boxes for one that is not
                               saved yet (it can be for a PC that is
                               already in the list, with another
                               display or resolution).
  Computer / Port / Password   Where to connect: the remote PC's name
                               or IP address. Tick "Remember password"
                               to keep this connection's password
                               (encrypted for your Windows account).
  Save                         Keeps what is in the boxes without
                               connecting. To rename a connection, pick
                               it, type the new name and click Save;
                               this works while connected too. A new
                               address for a connection is saved the
                               same way. Connecting saves as well.
  Forget                       Removes the connection named, and its
                               saved password, from the list.
  More                         Export connections: writes every saved
                               connection (name, computer, port,
                               password, resolution and display) to a
                               CSV file, as a backup or to carry to
                               another PC. The file holds the
                               remembered passwords as plain text, so
                               keep it somewhere only you can reach.
                               Import connections: adds the connections
                               in such a file to the list; one with the
                               name of a saved connection replaces it.
                               The file opens in Excel and can be
                               edited there; its columns are Name,
                               Computer, Port, Password, Resolution
                               (such as 1920x1080), Display (a number,
                               or "all"), then the connection's own
                               settings: Frames per second, Quality,
                               and yes or no for Sound, Allow sleep,
                               Copy and paste, Video for motion and
                               Fast capture.
  Remote display               Which of the remote PC's displays to
                               show, or all of them side by side. Each
                               connection remembers its own. The list follows
                               displays being plugged in or removed.
                               While the lock screen or an admin prompt
                               is up, all displays are shown, because
                               those can appear on any of them.
  Remote resolution            Sets the display being shown to another
                               resolution, for example one that suits
                               the screen you are sitting at. Choose it
                               before connecting or while connected;
                               each connection remembers its own and
                               applies it when it connects. Only
                               resolutions that display offers can be
                               used (the list shows them once you have
                               connected). The change is temporary: the
                               remote PC puts its display back a minute
                               after you disconnect, or at once if you
                               choose "Leave as it is". Not available
                               while all displays are shown together.
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

  DRAGGING FILES ONTO THE REMOTE SCREEN
  Drag files or folders from File Explorer on this PC and drop them on
  the remote screen. The remote pointer follows as you drag. When you
  let go, the files are offered to the remote PC the same way copied
  files are; once it has them, the client clicks the spot where you
  dropped them and presses Ctrl+V there, and Windows on the remote PC
  shows its copy progress window.

  - Drop on a folder window or the desktop and the files land there.
    Dropping on a folder's icon puts them in the folder that is open,
    not inside the icon's folder.
  - The click is a real click on the remote PC, so drop on the window
    you mean rather than on a button or a link.
  - It needs "Share copy and paste" ticked, and it replaces what is on
    the remote PC's clipboard.
  - With a server older than 1.7.0 the files reach the remote PC's
    clipboard but are not pasted for you: click where you want them and
    press Ctrl+V.
  - With several connections open, copy and paste is shared with the
    tab in front. When you switch tabs, what is on this PC's clipboard
    goes to the PC you switched to, so text or a picture copied on one
    remote PC can be pasted on another. Files copied on a remote PC can
    be pasted on this PC while that tab is in front, but not straight
    onto another remote PC: paste them here first, then copy them from
    here.

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
  - Every connected PC shares the one real screen; there are no
    separate desktops.

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
