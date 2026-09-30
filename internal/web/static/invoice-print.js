'use strict';
// The printable invoice's "Print / Save as PDF" button: the page's policy
// allows no inline script.
document.addEventListener('DOMContentLoaded', () => {
  document.querySelectorAll('.print-btn').forEach((b) => b.addEventListener('click', (e) => {
    e.preventDefault();
    window.print();
  }));
});
