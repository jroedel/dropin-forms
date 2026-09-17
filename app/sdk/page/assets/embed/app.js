/* The embedded form's own script. It runs inside the iframe.

   Two jobs, and the form works without either: the page is server-rendered
   and server-validated, so with JavaScript off it still submits, still
   validates, and still shows its errors. Everything here is an improvement on
   a working page rather than a requirement for one.

     1. Tell the parent how tall we are, so the frame is not a box with its own
        scrollbar inside somebody else's page.
     2. Hide the fields a condition says are not applicable, and say what is
        wrong with a field before the round trip.

   The parent half of the resize channel is embed.js, which the site owner
   pastes. This file is served from a content-hashed path, so it is cached
   forever and changes the moment it is edited. */

(function () {
  "use strict";

  // --- the resize channel -----------------------------------------------

  /* The origin to post to, which the server put here only after checking it
     against this form's own allowed-origins list. Never '*': that would
     broadcast our height, and whatever we ever add to the message, to whatever
     page happens to be framing us. Empty means the page was opened directly
     rather than embedded, or was framed by an origin this form does not
     permit -- in both cases there is nobody to tell.

     It is on an element rather than on <body> because the attribute is
     written by a page template and the layout is shared: html/template will
     not let a template write into an attribute-name position, which is the
     right restriction and not worth working around. */
  var root = document.getElementById("dropin-root");
  var parentOrigin = (root && root.getAttribute("data-parent-origin")) || "";

  var lastSent = -1;

  function tellParent() {
    if (!parentOrigin || window.parent === window) {
      return;
    }

    /* scrollHeight of the element, rather than of the document: the body is
       exactly as tall as its contents here, and documentElement.scrollHeight
       would include whatever height the frame already has -- which is how a
       resize loop that only ever grows begins. */
    var height = Math.ceil(document.body.getBoundingClientRect().height);

    /* A pixel of jitter, from a font loading or a focus ring, is not worth a
       message. Without this a hover that changes a border gets echoed to the
       parent on every mousemove. */
    if (Math.abs(height - lastSent) < 2) {
      return;
    }

    lastSent = height;

    window.parent.postMessage(
      { type: "dropin-forms:height", height: height },
      parentOrigin
    );
  }

  if (parentOrigin && window.parent !== window) {
    /* Anything that changes the layout: a conditional field appearing, a
       validation message, a web font arriving late. ResizeObserver rather than
       a poll, and it fires once on registration, which is also the initial
       measurement. */
    if (typeof ResizeObserver === "function") {
      new ResizeObserver(tellParent).observe(document.body);
    } else {
      window.addEventListener("load", tellParent);
      window.addEventListener("resize", tellParent);
    }

    /* Images and late fonts can change the height after everything else has
       settled, and a ResizeObserver on the body does catch those -- this is
       for the fallback path above. */
    window.addEventListener("load", tellParent);
  }

  // --- conditional fields -----------------------------------------------

  var form = document.querySelector("form[data-dropin-form]");
  if (!form) {
    return;
  }

  /* Every field that is only shown under a condition. The condition is in
     data attributes the server rendered from the same definition the server
     validates against, so the two cannot drift: there is one source and the
     browser is handed a copy of it rather than a second implementation. */
  var conditional = Array.prototype.slice.call(
    form.querySelectorAll("[data-show-if-field]")
  );

  function valuesOf(name) {
    var inputs = form.querySelectorAll("[name=\"" + cssEscape(name) + "\"]");
    var out = [];

    for (var i = 0; i < inputs.length; i++) {
      var el = inputs[i];

      if (el.type === "checkbox" || el.type === "radio") {
        if (el.checked) {
          out.push(el.value);
        }
      } else if (el.multiple && el.selectedOptions) {
        for (var j = 0; j < el.selectedOptions.length; j++) {
          out.push(el.selectedOptions[j].value);
        }
      } else if (el.value !== "") {
        out.push(el.value);
      }
    }

    return out;
  }

  /* CSS.escape where it exists, and a conservative fallback where it does not.
     Field names are already restricted to a slug-like alphabet by the
     definition loader, so this is belt and braces rather than the only thing
     standing between a field name and a selector injection. */
  function cssEscape(s) {
    if (window.CSS && typeof window.CSS.escape === "function") {
      return window.CSS.escape(s);
    }

    return String(s).replace(/["\\]/g, "\\$&");
  }

  function applyConditions() {
    for (var i = 0; i < conditional.length; i++) {
      var wrapper = conditional[i];
      var on = wrapper.getAttribute("data-show-if-field");
      var wanted = (wrapper.getAttribute("data-show-if-values") || "").split("\u001f");
      var have = valuesOf(on);

      var show = false;
      for (var j = 0; j < have.length && !show; j++) {
        show = wanted.indexOf(have[j]) !== -1;
      }

      wrapper.hidden = !show;

      /* A hidden field must not be validated by the browser, or the form
         cannot be submitted and nothing visible says why. The server does the
         same thing -- a field its condition hides has its requiredness
         suspended -- and this mirrors it rather than deciding it. */
      var inputs = wrapper.querySelectorAll("input, select, textarea");
      for (var k = 0; k < inputs.length; k++) {
        if (show) {
          if (inputs[k].hasAttribute("data-was-required")) {
            inputs[k].required = true;
            inputs[k].removeAttribute("data-was-required");
          }
        } else if (inputs[k].required) {
          inputs[k].required = false;
          inputs[k].setAttribute("data-was-required", "");
        }
      }
    }
  }

  if (conditional.length > 0) {
    form.addEventListener("change", applyConditions);
    form.addEventListener("input", applyConditions);
    applyConditions();
  }

  // --- saying what is wrong, before the round trip ----------------------

  /* The browser already knows: every constraint the definition expresses is
     rendered as a real HTML attribute, so validity is the platform's answer
     rather than a second copy of the rules. What is added here is where the
     message appears and that a screen reader hears it. */

  var live = form.querySelector("[data-live-summary]");

  function messageSlot(field) {
    var slot = field.parentNode.querySelector("[data-problem-for]");

    if (!slot) {
      slot = document.createElement("p");
      slot.className = "problem";
      slot.setAttribute("data-problem-for", field.name || "");
      field.parentNode.appendChild(slot);
    }

    return slot;
  }

  function showProblem(field) {
    if (!field.name || field.type === "hidden") {
      return;
    }

    var slot = messageSlot(field);

    if (field.validity.valid) {
      slot.textContent = "";
      field.removeAttribute("aria-invalid");

      return;
    }

    /* The browser's own sentence. It is localised, it is what the person would
       have seen in the bubble anyway, and inventing our own here would be a
       third wording of a rule that already has two. */
    slot.textContent = field.validationMessage;
    field.setAttribute("aria-invalid", "true");
  }

  form.addEventListener(
    "blur",
    function (e) {
      if (e.target.willValidate) {
        showProblem(e.target);
      }
    },
    true
  );

  form.addEventListener("input", function (e) {
    /* Only clear a message that is already showing. Validating on every
       keystroke tells somebody their address is invalid while they are still
       typing the first half of it. */
    if (e.target.getAttribute("aria-invalid") === "true") {
      showProblem(e.target);
    }
  });

  form.addEventListener("submit", function (e) {
    if (form.checkValidity()) {
      return;
    }

    e.preventDefault();

    var invalid = form.querySelectorAll(":invalid");
    var count = 0;

    for (var i = 0; i < invalid.length; i++) {
      if (invalid[i].willValidate) {
        showProblem(invalid[i]);
        count++;
      }
    }

    if (live) {
      live.textContent =
        count === 1
          ? "There is one answer to check before this can be sent."
          : "There are " + count + " answers to check before this can be sent.";
    }

    for (var j = 0; j < invalid.length; j++) {
      if (invalid[j].willValidate) {
        invalid[j].focus();
        break;
      }
    }
  });
})();
