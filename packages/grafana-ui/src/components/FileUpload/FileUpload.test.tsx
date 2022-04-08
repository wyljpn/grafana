import React from 'react';
import { shallow } from 'enzyme';
import { render, waitFor, fireEvent } from '@testing-library/react';
import { FileUpload } from './FileUpload';
import { selectors } from '@grafana/e2e-selectors';

describe('FileUpload', () => {
  it('should render upload button with default text and no file name', () => {
    const wrapper = shallow(<FileUpload onFileUpload={() => {}} />);
    expect(wrapper.findWhere((comp) => comp.text() === 'Upload file').exists()).toBeTruthy();
    expect(wrapper.find({ 'aria-label': 'File name' }).exists()).toBeFalsy();
  });

  it("should trim uploaded file's name", () => {
    const wrapper = shallow(<FileUpload onFileUpload={() => {}} />);

    wrapper.find('input').simulate('change', {
      currentTarget: {
        files: [{ name: 'longFileName.something.png' }],
      },
    });
    expect(wrapper.find({ 'aria-label': 'File name' }).exists()).toBeTruthy();
    // Trim file name longer than 16 chars
    expect(wrapper.find({ 'aria-label': 'File name' }).text()).toEqual('longFileName.som....png');

    // Keep the name below the length limit intact
    wrapper.find('input').simulate('change', {
      currentTarget: {
        files: [{ name: 'longFileName.png' }],
      },
    });
    expect(wrapper.find({ 'aria-label': 'File name' }).text()).toEqual('longFileName.png');
  });

  it('should display uploaded file name', async () => {
    const testFileName = 'grafana.png';
    const file = new File(['(⌐□_□)'], testFileName, { type: 'image/png' });
    const onFileUpload = jest.fn();
    const { getByTestId } = render(<FileUpload onFileUpload={onFileUpload} />);
    let uploader = getByTestId(selectors.components.FileUpload.inputField);
    await waitFor(() =>
      fireEvent.change(uploader, {
        target: { files: [file] },
      })
    );
    let uploaderLabel = getByTestId(selectors.components.FileUpload.fileNameSpan);
    expect(uploaderLabel).toHaveTextContent(testFileName);
  });
});
