# Open and Edit in KyDrive — 2026-11-15

`kypost-attachments-{before,after}-{desktop,phone}-{light,dark}.png`: the reader's attachment row,
captured with Playwright (Chromium, 1280×900 and 390×844, OS light and dark) on this branch's
production build. The KyPost API was answered by fixtures in the browser (one message with a docx,
an xlsx and a jpg; KyDrive status without `viewer` for before, with it for after), so no mail
server, KyDrive or sign-in was exercised. After: Open and Edit in KyDrive sit beside Save to KyDrive
on the two Office files; the photo gets neither. The viewer itself is in kydrive-server
`docs/ui-office-viewer-2026-11-15/`.
