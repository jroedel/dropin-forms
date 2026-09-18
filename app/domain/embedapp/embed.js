/* The snippet a site owner pastes, and the parent half of the resize channel.

   What goes into a Squarespace Code Block:

     <div data-dropin-form="feast-lunch-2026"></div>
     <script src="https://f.schoenstatt.link/embed.js" async></script>

   The div is not the form. It is replaced by an iframe pointing at this
   service, which means editing a form changes the live form with no re-paste,
   the card details never touch a page we wrote, and the host site's CSS cannot
   reach in and break the layout.

   Two things about Squarespace shape this file. Pages load over AJAX, so the
   div may appear after this script has already run and may be re-inserted on
   navigation -- hence the MutationObserver and the idempotence mark. And
   embedded scripts are disabled while you are logged in and editing, so the
   form will look broken in the editor and fine in a private window.

   This file also closes the payment round trip. Stripe sends the browser back
   to *this* page rather than to ours, with two parameters saying which form
   and what happened, and this script hands them to the frame so the thank-you
   appears where the form was. Without that the last thing that happens after
   paying is landing on the host page in front of a blank form, which reads as
   "did that work?" -- which is exactly how it read to the first person who
   paid. */

(function () {
  "use strict";

  /* Where this script was served from is where the forms are, so the snippet
     carries no second URL to keep in step. document.currentScript is null in
     a module or when the script is injected oddly, so there is a fallback. */
  var origin = (function () {
    var el = document.currentScript;
    if (el && el.src) {
      return new URL(el.src, window.location.href).origin;
    }

    var all = document.getElementsByTagName("script");
    for (var i = all.length - 1; i >= 0; i--) {
      if (all[i].src && all[i].src.indexOf("/embed.js") !== -1) {
        return new URL(all[i].src, window.location.href).origin;
      }
    }

    return "";
  })();

  /* What Stripe sent this page back with, read once at load.

     The names match paybus.ReturnMarker and paybus.ReturnState, which build
     the addresses Stripe is given; this is the third place those two words
     appear and the only one that is not Go. Keep them in step.

     Read once and kept, rather than read when a holder is mounted, because on
     Squarespace a holder can appear long after this script ran and the search
     string may have changed by then.

     The URL is deliberately left alone afterwards. Stripping the parameters
     with replaceState would mean a holder inserted later got a blank form,
     and a refresh showing the same acknowledgement again is better than a
     refresh replacing it with an empty form. Nothing in them is worth hiding:
     a form's name is public and the state is a word. */
  var RETURN_FORM = "dropin";
  var RETURN_STATE = "dropin_state";

  var returned = (function () {
    /* URLSearchParams is everywhere that matters, and where it is not, the
       return trip simply does not happen: the form still works, and somebody
       on a browser that old lands on the host page as before. */
    if (typeof URLSearchParams !== "function") {
      return null;
    }

    var q = new URLSearchParams(window.location.search);
    var slug = q.get(RETURN_FORM);
    var state = q.get(RETURN_STATE);

    /* Only the two words our own addresses carry. Anything else is not from
       us, and the frame is pointed at the ordinary form -- which is also what
       the server does with a state it does not recognise. */
    if (!slug || (state !== "paid" && state !== "cancelled")) {
      return null;
    }

    return { slug: slug, state: state };
  })();

  /* An iframe cannot be taller than this however tall the page inside claims
     to be. Without a clamp, a bug or a hostile response on our side becomes an
     element covering the owner's entire page -- and the owner is the one whose
     site looks broken. */
  var MAX_HEIGHT = 10000;
  var MIN_HEIGHT = 120;

  var frames = [];

  function mount(holder) {
    /* Idempotent, because the snippet may be pasted twice on a page and
       because Squarespace re-inserts nodes on AJAX navigation. */
    if (holder.getAttribute("data-dropin-mounted") === "1") {
      return;
    }

    var slug = holder.getAttribute("data-dropin-form");
    if (!slug || !origin) {
      return;
    }

    holder.setAttribute("data-dropin-mounted", "1");

    var frame = document.createElement("iframe");

    /* Either the form, or the acknowledgement of a payment when Stripe has
       just sent this page back here and the marker names this form. Matched
       on the slug, so a page carrying two forms shows the thank-you in the
       one that was paid for and a blank form in the other. */
    var path = "/f/" + encodeURIComponent(slug);

    if (returned && returned.slug === slug) {
      path += "/return?state=" + encodeURIComponent(returned.state) + "&";
    } else {
      path += "?";
    }

    /* The parent origin goes in as a parameter, and the service checks it
       against the form's own allowed-embedder list before the page inside
       will post anything to it. So this is a request rather than an
       instruction: asking for an origin the form does not permit gets a page
       that stays silent. */
    frame.src =
      origin +
      path +
      "parent=" +
      encodeURIComponent(window.location.origin);

    frame.title = holder.getAttribute("data-dropin-title") || "Form";
    frame.loading = "lazy";
    frame.style.width = "100%";
    frame.style.border = "0";
    frame.style.display = "block";
    frame.style.height = MIN_HEIGHT + "px";
    frame.setAttribute("scrolling", "no");

    /* allow="payment" is deliberately absent. Payment happens on Stripe's own
       page after a click that leaves this site, so nothing here needs the
       Payment Request API. When the in-page Payment Element eventually
       replaces that, this attribute is part of the change and wants testing
       through Squarespace's own nesting. */

    holder.appendChild(frame);
    frames.push(frame);
  }

  function mountAll() {
    var holders = document.querySelectorAll("[data-dropin-form]");
    for (var i = 0; i < holders.length; i++) {
      mount(holders[i]);
    }
  }

  window.addEventListener("message", function (event) {
    /* Four checks, and each one matters.

       The source, because a page with two forms on it gets two frames and the
       height has to go to the right one -- and because it is the only check
       that cannot be forged by a page that merely knows our origin.

       The origin, by exact string equality. A prefix test would accept
       https://f.schoenstatt.link.evil.example, which is a different site.

       The literal "null" origin, which a sandboxed frame or a data: URL sends
       and which matches nothing we would accept.

       And the payload: a known type, a finite integer, positive, clamped. */
    if (event.origin !== origin || event.origin === "null") {
      return;
    }

    var frame = null;
    for (var i = 0; i < frames.length; i++) {
      if (frames[i].contentWindow === event.source) {
        frame = frames[i];
        break;
      }
    }

    if (!frame) {
      return;
    }

    var msg = event.data;
    if (!msg || msg.type !== "dropin-forms:height") {
      return;
    }

    var height = msg.height;
    if (typeof height !== "number" || !isFinite(height) || height <= 0) {
      return;
    }

    height = Math.min(Math.max(Math.round(height), MIN_HEIGHT), MAX_HEIGHT);
    frame.style.height = height + "px";
  });

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", mountAll);
  } else {
    mountAll();
  }

  /* Squarespace swaps page content in without a document load, so a holder
     can appear long after this ran. Watching the whole subtree is broad, but
     mount() is cheap and idempotent and there is no narrower root to watch on
     a site whose markup is not ours. */
  if (typeof MutationObserver === "function") {
    new MutationObserver(function () {
      mountAll();
    }).observe(document.documentElement, { childList: true, subtree: true });
  }
})();
