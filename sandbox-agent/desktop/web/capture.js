// The two pictures a note is saved with.
//
// Kept out of app.js because they are the only part of the viewer that produces
// a durable artifact — everything else on that page is transient — and because
// pure canvas-in, PNG-out functions are the part worth testing directly. The
// e2e test in capture_e2e_test.go imports this module into a real browser and
// checks the pixels these draw.

// crop lifts the marked region out of the framebuffer. The canvas is painted by
// noVNC from the socket, so it is not tainted and can be read back; a browser
// that refuses anyway leaves the note without a picture rather than unsaved.
export function crop(canvas, region) {
  try {
    const out = document.createElement('canvas');
    out.width = region.w;
    out.height = region.h;
    out.getContext('2d').drawImage(
      canvas, region.x, region.y, region.w, region.h, 0, 0, region.w, region.h,
    );
    return out.toDataURL('image/png');
  } catch (err) {
    console.warn('could not read the framebuffer', err);
    return '';
  }
}

// screenWithRegion is the whole desktop as it was, with the marked rectangle
// drawn on it and everything outside it dimmed.
//
// It is saved beside the crop rather than instead of it, because neither
// answers for the other: a crop of a misaligned icon is unreadable as a
// location — it could be any toolbar on any window — and the whole desktop is
// too coarse to see two pixels of misalignment in. Read back weeks later, the
// crop says what was wrong and this says where it was.
//
// The dimming is the same gesture the marquee makes while the box is being
// dragged, so the saved picture looks like the moment it was taken.
export function screenWithRegion(canvas, region) {
  try {
    const out = document.createElement('canvas');
    out.width = canvas.width;
    out.height = canvas.height;
    const ctx = out.getContext('2d');
    ctx.drawImage(canvas, 0, 0);

    // Even-odd fill: the whole canvas minus the region, in one path.
    ctx.fillStyle = 'rgba(11, 9, 14, 0.55)';
    ctx.beginPath();
    ctx.rect(0, 0, out.width, out.height);
    ctx.rect(region.x, region.y, region.w, region.h);
    ctx.fill('evenodd');

    // Scaled to the framebuffer, so the outline is the same weight on a 2x
    // desktop as on a 1x one rather than a hairline on the larger.
    ctx.lineWidth = Math.max(2, Math.round(out.width / 640));
    ctx.strokeStyle = '#f45cff';
    ctx.strokeRect(
      region.x - ctx.lineWidth / 2,
      region.y - ctx.lineWidth / 2,
      region.w + ctx.lineWidth,
      region.h + ctx.lineWidth,
    );
    return out.toDataURL('image/png');
  } catch (err) {
    console.warn('could not read the framebuffer', err);
    return '';
  }
}
